package matrix

import (
	"context"
	"fmt"
	"html"
	"strings"
	"sync"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/rtc"
)

// callChange is what a membership event amounts to once the previous state is
// taken into account.
type callChange int

const (
	// callNoChange covers a membership that was already recorded either way:
	// state events are re-sent on every update, and only transitions matter.
	callNoChange callChange = iota
	callJoined
	callLeft
)

// callWatcher announces people joining and leaving the call.
//
// Call membership lives in room state, so the bot sees the changes as state
// events for free. The wrinkle is that a sync also replays the state that was
// already there: those must be recorded silently, or every restart would
// announce everyone currently in the call.
type callWatcher struct {
	mu sync.Mutex
	// present holds the membership state keys currently in the call.
	present map[string]bool
	// primed is false until the first sync has established who was already
	// there, during which memberships are recorded without announcing.
	primed bool
}

func newCallWatcher() *callWatcher {
	return &callWatcher{present: make(map[string]bool)}
}

// handleMembership records a membership change and reports the transition it
// represents, if any.
func (w *callWatcher) handleMembership(stateKey string, joined bool) callChange {
	w.mu.Lock()
	defer w.mu.Unlock()

	was := w.present[stateKey]
	if joined {
		w.present[stateKey] = true
	} else {
		delete(w.present, stateKey)
	}
	if !w.primed || joined == was {
		return callNoChange
	}
	if joined {
		return callJoined
	}
	return callLeft
}

// stillPresent reports whether the user behind a membership state key has any
// other membership in the call.
//
// One person can be in a call from several devices, and each device gets its
// own membership. Announcing a leave when only one of them dropped would be
// wrong, so the other keys are checked before saying someone left.
func (w *callWatcher) stillPresent(userID id.UserID) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for stateKey := range w.present {
		if strings.Contains(stateKey, userID.String()) {
			return true
		}
	}
	return false
}

// prime marks the initial state as seen, so subsequent joins are announced.
func (w *callWatcher) prime() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.primed = true
}

// handleCallMember is the sync handler for call membership state events.
func (b *Bot) handleCallMember(ctx context.Context, evt *event.Event) {
	if evt.RoomID != b.roomID || evt.StateKey == nil {
		return
	}
	// The bot announcing its own arrival would be noise.
	if evt.Sender == b.client.UserID {
		return
	}
	// The initial sync replays the last 50 timeline events, which includes any
	// call joins and leaves from before the bot started. Priming already
	// established who is actually in the call, so replaying that history would
	// only produce announcements for people who joined long ago.
	if b.isHistorical(evt) {
		return
	}

	// An empty content is a membership being retracted, i.e. a leave.
	joined := !isEmptyMembership(evt)
	change := b.calls.handleMembership(*evt.StateKey, joined)
	b.client.Log.Debug().
		Str("state_key", *evt.StateKey).
		Str("sender", evt.Sender.String()).
		Bool("joined", joined).
		Int("change", int(change)).
		Msg("call membership change")

	switch change {
	case callJoined:
		b.client.Log.Info().Str("user_id", evt.Sender.String()).Msg("joined the call")
		b.announcePresence(ctx, evt.Sender, "joined")
	case callLeft:
		// Someone in the call from two devices who closes one has not left.
		if b.calls.stillPresent(evt.Sender) {
			b.client.Log.Info().
				Str("user_id", evt.Sender.String()).
				Msg("left the call from one device, still present on another")
			return
		}
		b.client.Log.Info().Str("user_id", evt.Sender.String()).Msg("left the call")
		b.announcePresence(ctx, evt.Sender, "left")
	}
}

// announcePresence posts "<pill> joined/left the call".
func (b *Bot) announcePresence(ctx context.Context, user id.UserID, verb string) {
	name := b.displayName(ctx, user)
	plain := fmt.Sprintf("%s %s the call.", name, verb)
	formatted := fmt.Sprintf("%s %s the call.", userPill(user, name), verb)
	b.send(ctx, plain, formatted)
}

// userPill renders a matrix.to link, which clients display as a pill.
func userPill(user id.UserID, name string) string {
	if name == "" {
		name = user.String()
	}
	return fmt.Sprintf(`<a href="https://matrix.to/#/%s">%s</a>`,
		html.EscapeString(user.URI().MatrixToURL()[len("https://matrix.to/#/"):]),
		html.EscapeString(name))
}

// displayName resolves a user's room display name, falling back to the
// localpart of their ID.
func (b *Bot) displayName(ctx context.Context, user id.UserID) string {
	var member event.MemberEventContent
	if err := b.client.StateEvent(ctx, b.roomID, event.StateMember, user.String(), &member); err == nil {
		if name := strings.TrimSpace(member.Displayname); name != "" {
			return name
		}
	}
	localpart, _, err := user.Parse()
	if err != nil || localpart == "" {
		return user.String()
	}
	return localpart
}

// isEmptyMembership reports whether a membership event is a retraction. The
// raw bytes are the reliable signal: mautrix does not parse this unstable event
// type, so the parsed content is nil either way.
func isEmptyMembership(evt *event.Event) bool {
	if raw := evt.Content.VeryRaw; len(raw) > 0 {
		return isEmptyJSONObject(raw)
	}
	// No raw bytes retained: fall back to the decoded map.
	return len(evt.Content.Raw) == 0
}

// isEmptyJSONObject reports whether raw is `{}` once whitespace is ignored.
func isEmptyJSONObject(raw []byte) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed == "" || trimmed == "{}" || trimmed == "null"
}

// callMemberEventType is the state event the bot watches for participants.
var callMemberEventType = rtc.CallMemberEventType
