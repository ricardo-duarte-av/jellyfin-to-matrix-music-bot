package rtc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"maunium.net/go/mautrix"
)

// slotServer answers state reads with whatever content is set, and records what
// gets written back.
type slotServer struct {
	existing any
	// status is the response code for the state read; 404 means no such slot.
	status  int
	written map[string]any
	writes  int
	reject  bool
}

func (s *slotServer) client(t *testing.T) *mautrix.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if s.status == http.StatusNotFound {
				writeJSON(w, http.StatusNotFound, map[string]any{
					"errcode": "M_NOT_FOUND", "error": "Event not found.",
				})
				return
			}
			writeJSON(w, http.StatusOK, s.existing)
		case http.MethodPut:
			s.writes++
			_ = json.NewDecoder(r.Body).Decode(&s.written)
			if s.reject {
				writeJSON(w, http.StatusForbidden, map[string]any{
					"errcode": "M_FORBIDDEN", "error": "You don't have permission to post that to the room",
				})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"event_id": "$slot"})
		}
	}))
	t.Cleanup(srv.Close)
	client, err := mautrix.NewClient(srv.URL, "@bot:example.org", "token")
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// Re-sending state the room already holds is churn other clients react to.
func TestEnsureSlotLeavesAnOpenSlotAlone(t *testing.T) {
	srv := &slotServer{status: http.StatusOK, existing: map[string]any{
		"status": "open", "application": map[string]any{"type": "m.call"},
	}}
	if err := EnsureSlot(context.Background(), srv.client(t), "!room:example.org", DefaultSlotID); err != nil {
		t.Fatalf("EnsureSlot() = %v", err)
	}
	if srv.writes != 0 {
		t.Errorf("rewrote an already-open slot %d time(s)", srv.writes)
	}
}

// Every other shape means the slot is not usable and has to be opened: without
// an open slot, clients treat every member of it as having left.
func TestEnsureSlotOpensWhatIsNotOpen(t *testing.T) {
	for name, existing := range map[string]*slotServer{
		"absent":  {status: http.StatusNotFound},
		"closed":  {status: http.StatusOK, existing: map[string]any{"status": "closed"}},
		"other":   {status: http.StatusOK, existing: map[string]any{"status": "open", "application": map[string]any{"type": "m.thirdroom"}}},
		"garbage": {status: http.StatusOK, existing: map[string]any{}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := EnsureSlot(context.Background(), existing.client(t), "!room:example.org", DefaultSlotID); err != nil {
				t.Fatalf("EnsureSlot() = %v", err)
			}
			if existing.writes != 1 {
				t.Fatalf("wrote the slot %d time(s); want 1", existing.writes)
			}
			if existing.written["status"] != "open" {
				t.Errorf("wrote status %v; want open", existing.written["status"])
			}
			app, _ := existing.written["application"].(map[string]any)
			if app["type"] != "m.call" {
				t.Errorf("wrote application %+v; want m.call", app)
			}
		})
	}
}

// A rejection has to say what to do about it: the bot needs power to send this
// event type, and there is a config switch for rooms where it will not get it.
func TestEnsureSlotExplainsARejection(t *testing.T) {
	srv := &slotServer{status: http.StatusNotFound, reject: true}
	err := EnsureSlot(context.Background(), srv.client(t), "!room:example.org", DefaultSlotID)
	if err == nil {
		t.Fatal("EnsureSlot() = nil; want the rejection reported")
	}
	for _, want := range []string{"rtc.stack: legacy", SlotEventType.Type} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}
