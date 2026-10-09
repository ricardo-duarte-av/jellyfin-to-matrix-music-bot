package rtc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/rs/zerolog"
)

// A receiver holds only the raw key it was sent and derives the AES key the way
// Element Call's MatrixKeyProvider does. Frames the bot encrypts have to open
// with that, carrying the key index in their trailer.
func TestMediaKeysEncryptWhatAReceiverDecrypts(t *testing.T) {
	keys, err := NewMediaKeys()
	if err != nil {
		t.Fatal(err)
	}
	raw := bytes.Repeat([]byte{7}, MediaKeyBytes)
	if err := keys.Use(3, raw); err != nil {
		t.Fatal(err)
	}
	enc, err := keys.FrameEncryptor(lksdk.CodecOpus)
	if err != nil {
		t.Fatal(err)
	}

	frame := []byte{0xfc, 1, 2, 3, 4, 5, 6, 7, 8}
	sealed, err := enc.EncryptFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if kid := sealed[len(sealed)-1]; kid != 3 {
		t.Errorf("frame carries key index %d; want 3", kid)
	}
	if sealed[0] != frame[0] {
		t.Error("the Opus TOC byte must stay in the clear")
	}

	receiverKey, err := lksdk.DeriveKeyFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := lksdk.DecryptGCMAudioSample(sealed, receiverKey, nil)
	if err != nil {
		t.Fatalf("receiver could not decrypt: %v", err)
	}
	if !bytes.Equal(opened, frame) {
		t.Errorf("decrypted %x; want %x", opened, frame)
	}
}

// An encryptor switches keys when the index moves, with no rebuild.
func TestMediaKeysEncryptorFollowsTheCurrentKey(t *testing.T) {
	keys, err := NewMediaKeys()
	if err != nil {
		t.Fatal(err)
	}
	enc, err := keys.FrameEncryptor(lksdk.CodecOpus)
	if err != nil {
		t.Fatal(err)
	}
	if err := keys.Use(0, bytes.Repeat([]byte{1}, MediaKeyBytes)); err != nil {
		t.Fatal(err)
	}
	sealed, err := enc.EncryptFrame([]byte{0xfc, 9, 9, 9})
	if err != nil {
		t.Fatal(err)
	}
	if kid := sealed[len(sealed)-1]; kid != 0 {
		t.Errorf("still encrypting under index %d after switching to 0", kid)
	}
}

// sentKey is one call to the fake sender.
type sentKey struct {
	targets   []KeyTarget
	memberIDs []string
	index     int
	key       []byte
}

type fakeKeySender struct {
	mu   sync.Mutex
	sent []sentKey
	// unreach names devices to report as unreached.
	unreach map[KeyTarget]bool
}

func (f *fakeKeySender) send(_ context.Context, targets []KeyTarget, memberIDs []string, index int, key []byte) ([]KeyTarget, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentKey{targets: targets, memberIDs: memberIDs, index: index, key: key})
	var missed []KeyTarget
	for _, t := range targets {
		if f.unreach[KeyTarget{User: t.User, Device: t.Device}] {
			missed = append(missed, t)
		}
	}
	if len(missed) > 0 {
		return missed, errors.New("unreachable")
	}
	return nil, nil
}

func (f *fakeKeySender) take() []sentKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	sent := f.sent
	f.sent = nil
	return sent
}

// testKeyShare is a sharer applied by hand, on a clock the test moves.
func testKeyShare(t *testing.T) (*KeyShare, *MediaKeys, *fakeKeySender, *time.Time) {
	t.Helper()
	keys, err := NewMediaKeys()
	if err != nil {
		t.Fatal(err)
	}
	sender := &fakeKeySender{unreach: map[KeyTarget]bool{}}
	s := NewKeyShare(zerolog.New(io.Discard), keys, sender.send)
	now := time.Unix(1_000_000, 0)
	s.clock = func() time.Time { return now }
	s.useDelay = time.Hour // switched by hand in the tests that care
	return s, keys, sender, &now
}

func (s *KeyShare) applyNow(targets []KeyTarget, memberIDs ...string) {
	s.apply(context.Background(), keySnapshot{inCall: len(memberIDs) > 0, targets: targets, memberIDs: memberIDs})
}

var (
	alice = KeyTarget{User: "@alice:example.org", Device: "A", Session: "sticky:a1"}
	bob   = KeyTarget{User: "@bob:example.org", Device: "B", Session: "sticky:b1"}
)

// Entering the call makes a key and uses it at once: nothing was shared before
// it, so there is no older key anybody could still be relying on.
func TestKeyShareFirstKeyIsUsedAtOnce(t *testing.T) {
	s, keys, sender, _ := testKeyShare(t)
	s.applyNow([]KeyTarget{alice}, "member")

	sent := sender.take()
	if len(sent) != 1 || len(sent[0].targets) != 1 || sent[0].targets[0] != alice {
		t.Fatalf("sent %+v; want the key to alice", sent)
	}
	if got := keys.CurrentKeyIndex(); got != uint32(sent[0].index) {
		t.Errorf("encrypting under index %d; want the new key's %d at once", got, sent[0].index)
	}
}

// A newcomer while the key is young gets that key and nobody else is bothered.
func TestKeyShareHandsAYoungKeyToANewcomer(t *testing.T) {
	s, _, sender, now := testKeyShare(t)
	s.applyNow([]KeyTarget{alice}, "member")
	first := sender.take()[0]

	*now = now.Add(s.grace / 2)
	s.applyNow([]KeyTarget{alice, bob}, "member")
	sent := sender.take()
	if len(sent) != 1 || len(sent[0].targets) != 1 || sent[0].targets[0] != bob {
		t.Fatalf("sent %+v; want the current key to bob alone", sent)
	}
	if sent[0].index != first.index || !bytes.Equal(sent[0].key, first.key) {
		t.Error("bob got a different key from the one in use")
	}
}

// A newcomer to an old key means a new one for everybody, so they cannot
// decrypt what was played before they came.
func TestKeyShareRotatesForANewcomerToAnOldKey(t *testing.T) {
	s, keys, sender, now := testKeyShare(t)
	s.applyNow([]KeyTarget{alice}, "member")
	first := sender.take()[0]

	*now = now.Add(s.grace)
	s.applyNow([]KeyTarget{alice, bob}, "member")
	sent := sender.take()
	if len(sent) != 1 || len(sent[0].targets) != 2 {
		t.Fatalf("sent %+v; want a new key to both", sent)
	}
	if sent[0].index == first.index {
		t.Error("rotated without moving the key index")
	}
	if keys.CurrentKeyIndex() != uint32(first.index) {
		t.Error("switched to the new key before receivers could have it")
	}
}

// A departure means a new key for those who stay, so the leaver cannot decrypt
// what comes next.
func TestKeyShareRotatesWhenSomebodyLeaves(t *testing.T) {
	s, _, sender, _ := testKeyShare(t)
	s.applyNow([]KeyTarget{alice, bob}, "member")
	first := sender.take()[0]

	s.applyNow([]KeyTarget{alice}, "member")
	sent := sender.take()
	if len(sent) != 1 || len(sent[0].targets) != 1 || sent[0].targets[0] != alice {
		t.Fatalf("sent %+v; want a new key to alice alone", sent)
	}
	if sent[0].index == first.index {
		t.Error("kept the key the leaver has")
	}
}

// A client that rejoins has lost its keys even though its device never left.
func TestKeyShareTreatsANewSessionAsANewcomer(t *testing.T) {
	s, _, sender, _ := testKeyShare(t)
	s.applyNow([]KeyTarget{alice}, "member")
	sender.take()

	rejoined := alice
	rejoined.Session = "sticky:a2"
	s.applyNow([]KeyTarget{rejoined}, "member")
	if sent := sender.take(); len(sent) != 1 || sent[0].targets[0] != rejoined {
		t.Fatalf("sent %+v; want the key to alice's new session", sent)
	}
}

// Moving the bot onto another membership hands the same key out again under
// it: clients file keys by the membership they saw.
func TestKeyShareResendsWhenTheBotChangesMembership(t *testing.T) {
	s, _, sender, _ := testKeyShare(t)
	s.applyNow([]KeyTarget{alice, bob}, "legacy-member")
	first := sender.take()[0]

	s.applyNow([]KeyTarget{alice, bob}, "sticky-member")
	sent := sender.take()
	if len(sent) != 1 || len(sent[0].targets) != 2 {
		t.Fatalf("sent %+v; want the key to everybody again", sent)
	}
	if sent[0].index != first.index || sent[0].memberIDs[0] != "sticky-member" {
		t.Errorf("sent index %d under %v; want index %d under the new membership", sent[0].index, sent[0].memberIDs, first.index)
	}
}

// Leaving the call means a fresh key, used at once, on the way back in.
func TestKeyShareStartsOverAfterLeavingTheCall(t *testing.T) {
	s, keys, sender, _ := testKeyShare(t)
	s.applyNow([]KeyTarget{alice}, "member")
	first := sender.take()[0]

	s.applyNow(nil)
	s.applyNow([]KeyTarget{alice}, "member")
	sent := sender.take()
	if len(sent) != 1 || sent[0].index == first.index {
		t.Fatalf("sent %+v; want a new key on rejoining", sent)
	}
	if keys.CurrentKeyIndex() != uint32(sent[0].index) {
		t.Error("did not use the rejoining key at once")
	}
}

// A device the key did not reach is tried again with the same key. Treating it
// as a newcomer would rotate everybody's key on every attempt.
func TestKeyShareRetriesUnreachedDevicesWithoutRotating(t *testing.T) {
	s, _, sender, now := testKeyShare(t)
	sender.unreach[KeyTarget{User: bob.User, Device: bob.Device}] = true
	s.applyNow([]KeyTarget{alice, bob}, "member")
	first := sender.take()[0]

	// Not due yet: nothing happens.
	s.applyNow([]KeyTarget{alice, bob}, "member")
	if sent := sender.take(); len(sent) != 0 {
		t.Fatalf("sent %+v before the retry was due", sent)
	}

	delete(sender.unreach, KeyTarget{User: bob.User, Device: bob.Device})
	*now = now.Add(s.grace + s.retryDelay)
	s.applyNow([]KeyTarget{alice, bob}, "member")
	sent := sender.take()
	if len(sent) != 1 || len(sent[0].targets) != 1 || sent[0].targets[0] != bob {
		t.Fatalf("sent %+v; want a retry to bob alone", sent)
	}
	if sent[0].index != first.index {
		t.Error("rotated the key to retry one device")
	}

	// Reached now, so there is nothing left to retry.
	*now = now.Add(time.Hour)
	s.applyNow([]KeyTarget{alice, bob}, "member")
	if sent := sender.take(); len(sent) != 0 {
		t.Fatalf("sent %+v after bob was reached", sent)
	}
}

// Updates are applied by the worker.
func TestKeyShareRunAppliesUpdates(t *testing.T) {
	s, _, sender, _ := testKeyShare(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	s.Update([]KeyTarget{alice}, []string{"member"})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sender.mu.Lock()
		n := len(sender.sent)
		sender.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the worker never shared the key")
}
