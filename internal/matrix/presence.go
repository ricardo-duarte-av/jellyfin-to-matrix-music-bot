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

// callWatcher announces people joining the call.
//
// Call membership lives in room state, so the bot sees joins as state events
// for free. The wrinkle is that a sync also replays the state that was already
// there: those must be recorded silently, or every restart would announce
// everyone currently in the call.
type callWatcher struct {
	mu sync.Mutex
	// present holds the membership state keys currently in the call.
	present map[string]bool
	// primed is false until the first sync has established who was already
	// there, during which joins are recorded without announcing.
	primed bool
}

func newCallWatcher() *callWatcher {
	return &callWatcher{present: make(map[string]bool)}
}

// handleMembership records a membership change and reports whether it is a
// join worth announcing.
func (w *callWatcher) handleMembership(stateKey string, joined bool) (announce bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	was := w.present[stateKey]
	if joined {
		w.present[stateKey] = true
	} else {
		delete(w.present, stateKey)
	}
	return joined && !was && w.primed
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

	// An empty content is a membership being retracted, i.e. a leave.
	joined := len(evt.Content.VeryRaw) > 2 && !isEmptyJSONObject(evt.Content.VeryRaw)
	if !b.calls.handleMembership(*evt.StateKey, joined) {
		return
	}
	b.announceJoin(ctx, evt.Sender)
}

// announceJoin posts "<pill> joined the call".
func (b *Bot) announceJoin(ctx context.Context, user id.UserID) {
	name := b.displayName(ctx, user)
	plain := fmt.Sprintf("%s joined the call.", name)
	formatted := fmt.Sprintf(`%s joined the call.`, userPill(user, name))
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

// isEmptyJSONObject reports whether raw is `{}` once whitespace is ignored.
func isEmptyJSONObject(raw []byte) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed == "" || trimmed == "{}" || trimmed == "null"
}

// callMemberEventType is the state event the bot watches for participants.
var callMemberEventType = rtc.CallMemberEventType
