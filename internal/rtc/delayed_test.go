package rtc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

// delayServer answers delayed-event restarts with whatever it is told to.
type delayServer struct {
	mu sync.Mutex
	// status is the response code for a restart. 404 means the homeserver has
	// forgotten the delay, which is what happens once it has published it.
	status   int
	restarts []string
}

func (d *delayServer) client(t *testing.T) *mautrix.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		d.restarts = append(d.restarts, parts[len(parts)-1])
		if d.status == http.StatusNotFound {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"errcode": "M_NOT_FOUND", "error": "Delayed event not found",
			})
			return
		}
		if d.status != 0 && d.status != http.StatusOK {
			writeJSON(w, d.status, map[string]any{"errcode": "M_UNKNOWN", "error": "nope"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{})
	}))
	t.Cleanup(srv.Close)
	client, err := mautrix.NewClient(srv.URL, "@bot:example.org", "token")
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func (d *delayServer) seen() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.restarts...)
}

// A 404 means the homeserver has published our leave and forgotten the delay.
// Retrying that ID can only ever 404 again, so the keeper has to replace it —
// and the membership it belonged to has to be put back.
func TestKeeperReplacesAVanishedDelay(t *testing.T) {
	srv := &delayServer{status: http.StatusNotFound}
	client := srv.client(t)

	recovered := make(chan struct{}, 4)
	k := startDelayKeeper(client, "sticky", "OLD", 10*time.Millisecond,
		func(ctx context.Context) (id.DelayID, error) {
			select {
			case recovered <- struct{}{}:
			default:
			}
			srv.mu.Lock()
			srv.status = http.StatusOK
			srv.mu.Unlock()
			return "NEW", nil
		})
	defer k.Stop()

	select {
	case <-recovered:
	case <-time.After(2 * time.Second):
		t.Fatal("keeper never tried to recover from a vanished delay")
	}

	// From here on it must be refreshing the replacement, not the dead ID.
	deadline := time.After(2 * time.Second)
	for k.DelayID() != "NEW" {
		select {
		case <-deadline:
			t.Fatalf("keeper still holds delay %q; want NEW", k.DelayID())
		case <-time.After(5 * time.Millisecond):
		}
	}
	for {
		seen := srv.seen()
		if len(seen) > 0 && seen[len(seen)-1] == "NEW" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("keeper never restarted the replacement delay; saw %v", seen)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// Anything else is transient — a blip, a rate limit — and the timeout has slack
// for it. Rearming on those would churn the membership for no reason.
func TestKeeperKeepsTryingOnTransientErrors(t *testing.T) {
	srv := &delayServer{status: http.StatusInternalServerError}
	client := srv.client(t)

	var recoveries int
	var mu sync.Mutex
	k := startDelayKeeper(client, "legacy", "OLD", 10*time.Millisecond,
		func(ctx context.Context) (id.DelayID, error) {
			mu.Lock()
			recoveries++
			mu.Unlock()
			return "NEW", nil
		})
	defer k.Stop()

	deadline := time.After(2 * time.Second)
	for len(srv.seen()) < 3 {
		select {
		case <-deadline:
			t.Fatal("keeper stopped refreshing after a transient error")
		case <-time.After(5 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if recoveries != 0 {
		t.Errorf("re-armed %d time(s) over a transient error; want 0", recoveries)
	}
	if got := k.DelayID(); got != "OLD" {
		t.Errorf("DelayID() = %q; want the original to be kept", got)
	}
}

// Stopping has to be safe from a caller that holds nothing the recovery needs,
// and must not hang if a recovery is in flight.
func TestKeeperStopIsPromptWhileRecovering(t *testing.T) {
	srv := &delayServer{status: http.StatusNotFound}
	client := srv.client(t)

	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	k := startDelayKeeper(client, "sticky", "OLD", 5*time.Millisecond,
		func(ctx context.Context) (id.DelayID, error) {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			return "NEW", nil
		})

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery never started")
	}

	stopped := make(chan struct{})
	go func() {
		k.Stop()
		close(stopped)
	}()

	// Stop waits for the loop, which is inside the recovery: releasing it must
	// let Stop finish.
	close(release)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return after the recovery finished")
	}
}

// The timeout is the whole point of the dead man's switch, and the refresh has
// to fit inside it several times over: at one refresh per timeout, a single
// slow round trip is enough for the homeserver to publish the leave.
func TestDelayedLeaveHasSlack(t *testing.T) {
	if delayedLeaveRefresh*3 > delayedLeaveTimeout {
		t.Errorf("refresh %s against timeout %s leaves fewer than three attempts",
			delayedLeaveRefresh, delayedLeaveTimeout)
	}
	if delayedLeaveTimeout < 15*time.Second {
		t.Errorf("timeout %s is below the 15s MSC4143 suggests", delayedLeaveTimeout)
	}
}

// membershipServer is enough of a homeserver to arm, fire and send events.
type membershipServer struct {
	mu      sync.Mutex
	actions []string
	sent    []string
}

func (s *membershipServer) client(t *testing.T) *mautrix.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case strings.Contains(r.URL.Path, "/delayed_events/"):
			var body struct {
				Action string `json:"action"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.actions = append(s.actions, body.Action)
			writeJSON(w, http.StatusOK, map[string]any{})
		default:
			s.sent = append(s.sent, r.URL.Path)
			resp := map[string]any{"event_id": "$sent"}
			if r.URL.Query().Get("org.matrix.msc4140.delay") != "" {
				resp["delay_id"] = "DELAY1"
			}
			writeJSON(w, http.StatusOK, resp)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := mautrix.NewClient(srv.URL, "@bot:example.org", "token")
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// Leaving stops the keeper, and the keeper's recovery takes the membership's
// lock — so Leave must not hold that lock while it waits. Getting this wrong
// deadlocks shutdown, which only shows up as a bot that never lets go of a call.
func TestStickyLeaveDoesNotDeadlockOnTheKeeper(t *testing.T) {
	srv := &membershipServer{}
	client := srv.client(t)

	m, err := NewStickyMembership(client, "!room:example.org", DefaultSlotID, DefaultStickyDuration)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := m.Join(ctx); err != nil {
		t.Fatalf("Join() = %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- m.Leave(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Leave() = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Leave() deadlocked")
	}

	// The armed leave is fired rather than cancelled: that is the event we want
	// published.
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.actions) == 0 || srv.actions[len(srv.actions)-1] != "send" {
		t.Errorf("delayed event actions = %v; want the leave to be sent", srv.actions)
	}
}
