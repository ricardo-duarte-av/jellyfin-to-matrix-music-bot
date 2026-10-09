package matrix

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/sqlstatestore"

	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/config"
	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/rtc"
)

// The crypto and state stores have to come up on the pure-Go SQLite driver:
// mautrix's own default is cgo, and its schema is written against that one.
func TestCryptoStoresMigrateOnPureGoSQLite(t *testing.T) {
	ctx := context.Background()
	db, err := dbutil.NewWithDialect(sqliteURI(filepath.Join(t.TempDir(), "crypto.db")), "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	state := sqlstatestore.NewSQLStateStore(db, dbutil.ZeroLogger(zerolog.Nop()), false)
	if err := state.Upgrade(ctx); err != nil {
		t.Fatalf("state store upgrade: %v", err)
	}
	store := crypto.NewSQLCryptoStore(db, dbutil.ZeroLogger(zerolog.Nop()), "", "DEVICE", []byte("pickle"))
	if err := store.DB.Upgrade(ctx); err != nil {
		t.Fatalf("crypto store upgrade: %v", err)
	}

	// And they work: the encryption flag the album art upload reads survives
	// a round trip.
	room := id.RoomID("!room:example.org")
	if err := state.SetEncryptionEvent(ctx, room, &event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1}); err != nil {
		t.Fatal(err)
	}
	if encrypted, err := state.IsEncrypted(ctx, room); err != nil || !encrypted {
		t.Errorf("IsEncrypted() = %v, %v; want true", encrypted, err)
	}
}

// A membership that arrives encrypted gets its stickiness back once decrypted,
// or it would look like one with no lifetime and be thrown away.
func TestDecryptedEventsKeepTheirStickiness(t *testing.T) {
	client, err := mautrix.NewClient("https://example.org", "@bot:example.org", "token")
	if err != nil {
		t.Fatal(err)
	}
	c := &Crypto{client: client, sticky: make(map[id.EventID]stickyNote)}

	var got *event.Event
	client.Syncer.(*mautrix.DefaultSyncer).OnEventType(rtc.StickyMemberEventType, func(_ context.Context, evt *event.Event) {
		got = evt
	})

	encrypted := &event.Event{ID: "$m", Type: event.EventEncrypted, Sticky: &event.Sticky{}}
	encrypted.Sticky.Duration.Duration = 10 * time.Minute
	c.rememberSticky(context.Background(), encrypted)

	decrypted := &event.Event{ID: "$m", Type: rtc.StickyMemberEventType}
	c.dispatchDecrypted(context.Background(), decrypted)
	if got == nil {
		t.Fatal("decrypted event was not dispatched")
	}
	if got.Sticky.GetDuration() != 10*time.Minute {
		t.Errorf("sticky duration %v; want 10m", got.Sticky.GetDuration())
	}
	if len(c.sticky) != 0 {
		t.Error("kept the note after using it")
	}
}

// The key content is what Element Call and xmuks parse.
func TestMediaKeyContentShape(t *testing.T) {
	raw, err := json.Marshal(&mediaKeyContent{
		Keys:    mediaKeyEntry{Index: 2, Key: "a2V5"},
		RoomID:  "!room:example.org",
		Member:  mediaKeyMember{ClaimedDeviceID: "BOT", ID: "member"},
		Session: mediaKeySession{Application: "m.call", Scope: "m.room"},
		SentTS:  1234,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"keys":{"index":2,"key":"a2V5"},"room_id":"!room:example.org",` +
		`"member":{"claimed_device_id":"BOT","id":"member"},` +
		`"session":{"call_id":"","application":"m.call","scope":"m.room"},"sent_ts":1234}`
	if string(raw) != want {
		t.Errorf("content =\n%s\nwant\n%s", raw, want)
	}
}

// The media key goes to every device in the call but the bot's, once per
// device whichever dialects it is on, and only to devices it can be addressed
// to.
func TestKeyTargets(t *testing.T) {
	const self = id.UserID("@bot:example.org")
	const alice = id.UserID("@alice:example.org")
	now := time.Now()
	w := primedWatcher()
	join := func(evt *event.Event) {
		w.applySticky(evt, true, now.Add(10*time.Minute), string(evt.ID), now)
	}

	// The bot itself, on both dialects.
	w.applyLegacy(self, "_@bot:example.org_BOT_m.call", true)
	join(stickyMemberOn(t, self, "BOT", "$self", now))
	// Bob on both dialects from one laptop.
	w.applyLegacy(bob, "_@bob:example.org_LAPTOP_m.call", true)
	w.noteLegacySession("_@bob:example.org_LAPTOP_m.call", "100")
	join(stickyMemberOn(t, bob, "LAPTOP", "$bob", now))
	// Alice on legacy only, from a phone.
	w.applyLegacy(alice, "_@alice:example.org_PHONE_m.call", true)
	w.noteLegacySession("_@alice:example.org_PHONE_m.call", "200")
	// A membership that names no device cannot be sent anything.
	w.applyLegacy(alice, "@alice:example.org", true)

	got := w.keyTargets(self, now)
	want := []rtc.KeyTarget{
		{User: alice, Device: "PHONE", Session: "legacy:200"},
		{User: bob, Device: "LAPTOP", Session: "legacy:100|sticky:$bob"},
	}
	if len(got) != len(want) {
		t.Fatalf("keyTargets = %+v; want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("keyTargets[%d] = %+v; want %+v", i, got[i], want[i])
		}
	}
}

// A legacy membership's visit is its created_ts, which renewals keep and a
// rejoin changes.
func TestLegacySession(t *testing.T) {
	withTS := &event.Event{ID: "$a"}
	withTS.Content.VeryRaw = json.RawMessage(`{"device_id":"D","created_ts":1700000000000}`)
	if got := legacySession(withTS); got != "1700000000000" {
		t.Errorf("legacySession = %q; want the created_ts", got)
	}
	without := &event.Event{ID: "$b"}
	without.Content.VeryRaw = json.RawMessage(`{"device_id":"D"}`)
	if got := legacySession(without); got != "$b" {
		t.Errorf("legacySession = %q; want the event ID", got)
	}
}

// Starting encryption on a fresh store creates the Olm account with the pure-Go
// implementation, uploads its keys, and leaves the sync token in memory.
func TestSetupCryptoOnAFreshStore(t *testing.T) {
	var uploaded bool
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/keys/query":
			_, _ = w.Write([]byte(`{"device_keys":{}}`))
		case "/_matrix/client/v3/keys/upload":
			uploaded = true
			_, _ = w.Write([]byte(`{"one_time_key_counts":{"signed_curve25519":50}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer hs.Close()

	client, err := mautrix.NewClient(hs.URL, "@bot:example.org", "token")
	if err != nil {
		t.Fatal(err)
	}
	client.DeviceID = "BOT"
	c, err := SetupCrypto(context.Background(), client, config.Crypto{
		Store:     filepath.Join(t.TempDir(), "crypto.db"),
		PickleKey: "pickle",
	})
	if err != nil {
		t.Fatalf("SetupCrypto() = %v", err)
	}
	defer c.Close()

	if !uploaded {
		t.Error("never uploaded the device keys")
	}
	if client.Crypto == nil {
		t.Error("messages would not be encrypted: client.Crypto is unset")
	}
	if _, ok := client.Store.(volatileSyncStore); !ok {
		t.Errorf("sync store is %T; the sync token would persist across restarts", client.Store)
	}
}
