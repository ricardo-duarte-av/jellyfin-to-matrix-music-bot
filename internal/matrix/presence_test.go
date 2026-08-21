package matrix

import (
	"encoding/json"
	"testing"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/rtc"
)

func stickyMember(t *testing.T, sender id.UserID, eventID, stickyKey, membership string, ts int64, duration time.Duration) *event.Event {
	t.Helper()
	content := rtc.StickyMemberContent{
		SlotID:    rtc.DefaultSlotID,
		Member:    rtc.StickyMemberInfo{ID: stickyKey, Membership: membership},
		StickyKey: stickyKey,
	}
	raw, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	evt := &event.Event{
		ID:        id.EventID(eventID),
		Sender:    sender,
		Type:      rtc.StickyMemberEventType,
		Timestamp: ts,
		Sticky:    &event.Sticky{},
	}
	evt.Sticky.Duration.Duration = duration
	evt.Content.VeryRaw = raw
	return evt
}

const bob = id.UserID("@bob:example.org")

func primedWatcher() *callWatcher {
	w := newCallWatcher()
	w.prime()
	w.primeSticky()
	return w
}

func TestStickyJoinAndLeave(t *testing.T) {
	w := primedWatcher()
	now := time.Now()

	join := stickyMember(t, bob, "$1", "MEMBER", "join", now.UnixMilli(), 10*time.Minute)
	if got := w.applySticky(join, true, now.Add(10*time.Minute), "MEMBER", now); got != callJoined {
		t.Fatalf("applySticky(join) = %v; want callJoined", got)
	}

	leave := stickyMember(t, bob, "$2", "MEMBER", "leave", now.Add(time.Second).UnixMilli(), 10*time.Minute)
	if got := w.applySticky(leave, false, now.Add(11*time.Minute), "MEMBER", now); got != callLeft {
		t.Fatalf("applySticky(leave) = %v; want callLeft", got)
	}
}

// The same event arrives in both the timeline and the sticky section of /sync,
// and either may land first. Announcing twice would be noise.
func TestStickyDuplicateDeliveryIsSilent(t *testing.T) {
	w := primedWatcher()
	now := time.Now()
	join := stickyMember(t, bob, "$1", "MEMBER", "join", now.UnixMilli(), 10*time.Minute)

	if got := w.applySticky(join, true, now.Add(10*time.Minute), "MEMBER", now); got != callJoined {
		t.Fatalf("first delivery = %v; want callJoined", got)
	}
	if got := w.applySticky(join, true, now.Add(10*time.Minute), "MEMBER", now); got != callNoChange {
		t.Errorf("second delivery = %v; want callNoChange", got)
	}
}

// MSC4354 tie-break: for one sticky key the event that expires last wins, so a
// stale refresh arriving late must not undo a leave.
func TestStickyTieBreakKeepsTheLastToExpire(t *testing.T) {
	w := primedWatcher()
	now := time.Now()

	// A leave with a long stickiness, then an older join that expires sooner.
	leave := stickyMember(t, bob, "$2", "MEMBER", "leave", now.UnixMilli(), 30*time.Minute)
	w.applySticky(leave, false, now.Add(30*time.Minute), "MEMBER", now)

	stale := stickyMember(t, bob, "$1", "MEMBER", "join", now.UnixMilli(), 10*time.Minute)
	if got := w.applySticky(stale, true, now.Add(10*time.Minute), "MEMBER", now); got != callNoChange {
		t.Errorf("stale join = %v; want it ignored", got)
	}
	if w.stillPresent(bob) {
		t.Error("a stale join resurrected a member who had left")
	}
}

func TestStickyTieBreakOnEventID(t *testing.T) {
	w := primedWatcher()
	now := time.Now()
	ts := now.UnixMilli()

	high := stickyMember(t, bob, "$zzz", "MEMBER", "join", ts, 10*time.Minute)
	w.applySticky(high, true, now.Add(10*time.Minute), "MEMBER", now)

	// Same score, lower event ID: the higher ID stands.
	low := stickyMember(t, bob, "$aaa", "MEMBER", "leave", ts, 10*time.Minute)
	if got := w.applySticky(low, false, now.Add(10*time.Minute), "MEMBER", now); got != callNoChange {
		t.Errorf("lower event ID = %v; want it ignored", got)
	}
	if !w.stillPresent(bob) {
		t.Error("a lower event ID displaced the winning membership")
	}
}

// A member who crashes sends nothing at all: expiry is the only leave signal.
func TestStickyExpirySweepAnnouncesTheLeave(t *testing.T) {
	w := primedWatcher()
	now := time.Now()
	join := stickyMember(t, bob, "$1", "MEMBER", "join", now.UnixMilli(), time.Minute)
	w.applySticky(join, true, now.Add(time.Minute), "MEMBER", now)

	if left := w.expireSticky(now.Add(30 * time.Second)); len(left) != 0 {
		t.Fatalf("expireSticky() = %v before expiry; want none", left)
	}
	left := w.expireSticky(now.Add(2 * time.Minute))
	if len(left) != 1 || left[0] != bob {
		t.Fatalf("expireSticky() = %v; want [%s]", left, bob)
	}
	if w.stillPresent(bob) {
		t.Error("stillPresent() = true after the membership lapsed")
	}
}

// Until the first sync has been recorded, everything already in flight is
// existing state rather than an arrival.
func TestStickyMembershipsBeforePrimingAreSilent(t *testing.T) {
	w := newCallWatcher()
	w.prime()
	now := time.Now()

	join := stickyMember(t, bob, "$1", "MEMBER", "join", now.UnixMilli(), 10*time.Minute)
	if got := w.applySticky(join, true, now.Add(10*time.Minute), "MEMBER", now); got != callNoChange {
		t.Errorf("unprimed join = %v; want callNoChange", got)
	}
	if !w.stillPresent(bob) {
		t.Error("an unprimed join was not recorded")
	}
}

// The bot itself publishes both dialects at once, and so will every client
// during the transition. Someone visible twice is still one person.
func TestPresenceIsAnnouncedOncePerPerson(t *testing.T) {
	w := primedWatcher()
	now := time.Now()

	if got := w.applyLegacy(bob, "_@bob:example.org_DEVICE", true); got != callJoined {
		t.Fatalf("legacy join = %v; want callJoined", got)
	}
	join := stickyMember(t, bob, "$1", "MEMBER", "join", now.UnixMilli(), 10*time.Minute)
	if got := w.applySticky(join, true, now.Add(10*time.Minute), "MEMBER", now); got != callNoChange {
		t.Errorf("sticky join by an already-present member = %v; want callNoChange", got)
	}

	// Dropping one dialect is not leaving while the other still says otherwise.
	if got := w.applyLegacy(bob, "_@bob:example.org_DEVICE", false); got != callNoChange {
		t.Errorf("legacy leave with a live sticky membership = %v; want callNoChange", got)
	}
	leave := stickyMember(t, bob, "$2", "MEMBER", "leave", now.Add(time.Second).UnixMilli(), 10*time.Minute)
	if got := w.applySticky(leave, false, now.Add(11*time.Minute), "MEMBER", now); got != callLeft {
		t.Errorf("final leave = %v; want callLeft", got)
	}
}

// Ejecting a sticky membership means redacting its event, because the ephemeral
// map is keyed by sender: a leave published by the bot would be its own entry.
func TestStickyEventsOfListsRedactionTargets(t *testing.T) {
	w := primedWatcher()
	now := time.Now()

	w.applySticky(stickyMember(t, bob, "$laptop", "LAPTOP", "join", now.UnixMilli(), 10*time.Minute),
		true, now.Add(10*time.Minute), "LAPTOP", now)
	w.applySticky(stickyMember(t, bob, "$phone", "PHONE", "join", now.UnixMilli(), 10*time.Minute),
		true, now.Add(10*time.Minute), "PHONE", now)
	w.applySticky(stickyMember(t, "@alice:example.org", "$alice", "A", "join", now.UnixMilli(), 10*time.Minute),
		true, now.Add(10*time.Minute), "A", now)

	got := w.stickyEventsOf(bob, now)
	want := []id.EventID{"$laptop", "$phone"}
	if len(got) != len(want) {
		t.Fatalf("stickyEventsOf() = %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stickyEventsOf() = %v; want %v", got, want)
		}
	}

	w.forgetSticky(bob)
	if w.stillPresent(bob) {
		t.Error("forgetSticky() left the ejected member in the call")
	}
	if !w.stillPresent("@alice:example.org") {
		t.Error("forgetSticky() removed somebody else's membership")
	}
}
