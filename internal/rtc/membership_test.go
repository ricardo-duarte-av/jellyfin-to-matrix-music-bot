package rtc

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
)

// The class is load-bearing. mautrix keys both its sync listener map and its
// room state map by the whole event.Type struct, and stamps StateEventType onto
// incoming state events. A type whose class is UnknownEventType — which is what
// event.NewEventType returns for any unstable type — silently matches nothing:
// sync handlers never fire and state lookups return empty.
func TestCallMemberEventTypeIsAStateEvent(t *testing.T) {
	if got := CallMemberEventType.Class; got != event.StateEventType {
		t.Errorf("CallMemberEventType.Class = %v; want StateEventType", got)
	}
	if got := CallMemberEventType.Type; got != "org.matrix.msc3401.call.member" {
		t.Errorf("CallMemberEventType.Type = %q; unexpected", got)
	}

	// Guard the specific trap: constructing it the obvious way is wrong, and
	// the two must not be interchangeable.
	guessed := event.NewEventType("org.matrix.msc3401.call.member")
	if guessed == CallMemberEventType {
		t.Skip("mautrix now recognises this type; the explicit class is redundant")
	}
	if guessed.Class == event.StateEventType {
		t.Error("NewEventType now infers the state class; this test needs revisiting")
	}
}

// A membership keyed the way mautrix keys incoming state events must be found
// by a lookup using our type.
func TestCallMemberEventTypeMatchesStateMapKey(t *testing.T) {
	incoming := event.Type{Type: "org.matrix.msc3401.call.member"}
	incoming.Class = event.StateEventType // what mautrix does on receipt

	state := map[event.Type]map[string]*event.Event{
		incoming: {"_@someone:example.org_DEV": {}},
	}
	if _, ok := state[CallMemberEventType]; !ok {
		t.Error("lookup with CallMemberEventType missed a state map keyed as mautrix keys it")
	}
}

// Ejecting someone means retracting every membership they hold, so the key
// formats in the wild all have to be recognised — without one user's ID
// swallowing another whose ID starts the same way.
func TestMembershipBelongsTo(t *testing.T) {
	const bob = "@bob:example.org"
	for _, key := range []string{
		"_@bob:example.org_DEVICE",
		"@bob:example.org_DEVICE",
		"@bob:example.org",
	} {
		if !MembershipBelongsTo(key, bob) {
			t.Errorf("MembershipBelongsTo(%q, %s) = false; want true", key, bob)
		}
	}
	for _, key := range []string{
		"_@bobby:example.org_DEVICE",
		"@bobby:example.org",
		"_@alice:example.org_DEVICE",
		"",
	} {
		if MembershipBelongsTo(key, bob) {
			t.Errorf("MembershipBelongsTo(%q, %s) = true; want false", key, bob)
		}
	}
}

// stateRoom serves the one state event a membership reads back and records what
// gets published to it.
type stateRoom struct {
	mu        sync.Mutex
	published []SessionMembership
	// current is what a read returns; nil means the homeserver has no such
	// event, which is what a fired delayed leave leaves behind.
	current *SessionMembership
}

func (s *stateRoom) client(t *testing.T) *mautrix.Client {
	t.Helper()
	mux := http.NewServeMux()
	path := "/_matrix/client/v3/rooms/{roomID}/state/org.matrix.msc3401.call.member/{stateKey}"
	mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.current == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"errcode": "M_NOT_FOUND", "error": "Event not found.",
			})
			return
		}
		writeJSON(w, http.StatusOK, s.current)
	})
	mux.HandleFunc("PUT "+path, func(w http.ResponseWriter, r *http.Request) {
		var content SessionMembership
		_ = json.NewDecoder(r.Body).Decode(&content)
		s.mu.Lock()
		s.published = append(s.published, content)
		s.current = &content
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"event_id": "$published"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
		writeJSON(w, http.StatusNotFound, map[string]any{"errcode": "M_UNRECOGNIZED"})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client, err := mautrix.NewClient(srv.URL, "@bot:example.org", "token")
	if err != nil {
		t.Fatal(err)
	}
	client.Log = zerolog.New(io.Discard)
	return client
}

func (s *stateRoom) publishedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.published)
}

// joinedMembership is a Membership in the state Join leaves it in, without the
// delayed-leave machinery a real join sets up.
func joinedMembership(client *mautrix.Client) *Membership {
	m := NewMembership(client, "!room:example.org", "@bot:example.org", "DEVICE")
	m.joined = true
	m.content = &SessionMembership{
		Application:  applicationCall,
		DeviceID:     "DEVICE",
		CreatedTS:    12345,
		Scope:        scopeRoom,
		MembershipID: m.MembershipID(),
	}
	return m
}

// A membership that vanished — the homeserver restarted and published the
// delayed leave, say — has to come back, or the bot streams to a room that
// believes it left.
func TestReconcileRepublishesAMissingMembership(t *testing.T) {
	room := &stateRoom{}
	m := joinedMembership(room.client(t))

	m.reconcile(context.Background())

	if room.publishedCount() != 1 {
		t.Fatalf("published %d memberships; want the missing one re-sent", room.publishedCount())
	}
	// created_ts decides focus ordering, so the membership must come back
	// exactly as it was rather than as a fresh join.
	if got := room.published[0].CreatedTS; got != 12345 {
		t.Errorf("re-published created_ts = %d; want the original 12345", got)
	}
}

// An emptied-out membership is how a leave is published: an event that is there
// but says nothing counts as gone.
func TestReconcileRepublishesAnEmptiedMembership(t *testing.T) {
	room := &stateRoom{current: &SessionMembership{}}
	m := joinedMembership(room.client(t))

	m.reconcile(context.Background())

	if room.publishedCount() != 1 {
		t.Errorf("published %d memberships; want the emptied one re-sent", room.publishedCount())
	}
}

// The common case is a membership that is exactly where it was left, and a
// state event every minute must not turn into a state event every minute.
func TestReconcileLeavesAHealthyMembershipAlone(t *testing.T) {
	room := &stateRoom{current: &SessionMembership{Application: applicationCall, DeviceID: "DEVICE"}}
	m := joinedMembership(room.client(t))

	m.reconcile(context.Background())

	if room.publishedCount() != 0 {
		t.Errorf("re-published %d memberships; want none while ours is still there", room.publishedCount())
	}
}

// A membership can rot where it stands: the state event never expires on its
// own, so the homeserver keeps serving it while every client applies the
// expires field and drops the bot from the call. Nothing else renews it.
func TestReconcileRenewsAMembershipThatIsAboutToExpire(t *testing.T) {
	created := time.Now().Add(-4 * time.Hour)
	// Published, healthy, and five minutes from stopping to count.
	content := SessionMembership{
		Application: applicationCall,
		DeviceID:    "DEVICE",
		CreatedTS:   created.UnixMilli(),
		Expires:     (4*time.Hour + 5*time.Minute).Milliseconds(),
	}
	onTheServer := content
	room := &stateRoom{current: &onTheServer}
	m := joinedMembership(room.client(t))
	m.content = &content

	m.reconcile(context.Background())

	if room.publishedCount() != 1 {
		t.Fatalf("published %d memberships; want the expiring one renewed", room.publishedCount())
	}
	renewed := room.published[0]
	// created_ts decides focus ordering, so renewing must extend the lifetime
	// rather than move its origin.
	if renewed.CreatedTS != created.UnixMilli() {
		t.Errorf("renewed created_ts = %d; want the original %d", renewed.CreatedTS, created.UnixMilli())
	}
	expiresAt := renewed.CreatedTS + renewed.Expires
	if want := time.Now().Add(membershipExpiry - time.Minute).UnixMilli(); expiresAt < want {
		t.Errorf("renewed membership expires at %d; want a full window, past %d", expiresAt, want)
	}

	// The renewal has to stick to what is cached, or recover would put the
	// stale lifetime back the next time it re-sends the membership.
	m.mu.Lock()
	cached := m.content.Expires
	m.mu.Unlock()
	if cached != renewed.Expires {
		t.Errorf("cached expires = %d; want the renewed %d", cached, renewed.Expires)
	}
}

// Renewing every minute would be as bad as never renewing: a membership with
// most of its lifetime ahead of it is left alone.
func TestReconcileLeavesAMembershipWithTimeLeftAlone(t *testing.T) {
	content := SessionMembership{
		Application: applicationCall,
		DeviceID:    "DEVICE",
		CreatedTS:   time.Now().UnixMilli(),
		Expires:     membershipExpiry.Milliseconds(),
	}
	onTheServer := content
	room := &stateRoom{current: &onTheServer}
	m := joinedMembership(room.client(t))
	m.content = &content

	m.reconcile(context.Background())

	if room.publishedCount() != 0 {
		t.Errorf("re-published %d memberships; want none while ours has hours left", room.publishedCount())
	}
}

// A membership that went missing after its lifetime ran out must come back
// alive. Re-sending it verbatim would republish something every client reads
// as already expired.
func TestReconcileRepublishesAMissingMembershipWithALiveExpiry(t *testing.T) {
	room := &stateRoom{}
	m := joinedMembership(room.client(t))
	m.content.CreatedTS = time.Now().Add(-6 * time.Hour).UnixMilli()
	m.content.Expires = (4 * time.Hour).Milliseconds()

	m.reconcile(context.Background())

	if room.publishedCount() != 1 {
		t.Fatalf("published %d memberships; want the missing one re-sent", room.publishedCount())
	}
	republished := room.published[0]
	if isExpired(&event.Event{}, &republished) {
		t.Errorf("re-published a membership that expired at %d; now is %d",
			republished.CreatedTS+republished.Expires, time.Now().UnixMilli())
	}
}

// Nothing is republished after a deliberate leave.
func TestReconcileDoesNothingWhenNotJoined(t *testing.T) {
	room := &stateRoom{}
	m := NewMembership(room.client(t), "!room:example.org", "@bot:example.org", "DEVICE")

	m.reconcile(context.Background())

	if room.publishedCount() != 0 {
		t.Errorf("published %d memberships while not in the call; want none", room.publishedCount())
	}
}
