package matrix

import (
	"context"
	"fmt"
	"html"
	"slices"
	"strings"
	"sync"
	"time"

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
	// sticky holds MSC4143 memberships, keyed by sender and sticky key. These
	// are message events rather than state, so they are tracked separately and
	// expire on a timer instead of being retracted.
	sticky map[stickyHandle]*stickyEntry
	// primed is false until the first sync has established who was already
	// there, during which memberships are recorded without announcing.
	primed bool
	// stickyPrimed is the same for sticky memberships, which arrive through
	// /sync rather than through room state and so are primed separately.
	stickyPrimed bool
}

// stickyHandle is the part of the MSC4354 ephemeral map key that varies within
// one room and event type.
type stickyHandle struct {
	sender    id.UserID
	stickyKey string
}

// stickyEntry is one live sticky membership, with what is needed to decide
// whether a later event supersedes it and when it lapses on its own.
type stickyEntry struct {
	joined    bool
	expiresAt time.Time
	// score is origin_server_ts + sticky duration: the later it is, the longer
	// this event will outlive its rivals, and the MSC4354 tie-break says the
	// last to expire wins.
	score   int64
	eventID string
}

func newCallWatcher() *callWatcher {
	return &callWatcher{
		present: make(map[string]bool),
		sticky:  make(map[stickyHandle]*stickyEntry),
	}
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

// applyLegacy records a session-style membership change and reports what it
// means for the sender's presence overall, across both dialects.
func (w *callWatcher) applyLegacy(userID id.UserID, stateKey string, joined bool) callChange {
	w.mu.Lock()
	defer w.mu.Unlock()

	now := time.Now()
	before := w.handlesLocked(userID, now)
	if joined {
		w.present[stateKey] = true
	} else {
		delete(w.present, stateKey)
	}
	after := w.handlesLocked(userID, now)

	if !w.primed {
		return callNoChange
	}
	return transition(before, after)
}

// stillPresent reports whether a user is in the call by any route at all.
//
// One person can be in a call from several devices, each with its own
// membership, and during the MatrixRTC transition from several dialects at
// once. Announcing a leave when only one of those dropped would be wrong.
func (w *callWatcher) stillPresent(userID id.UserID) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.handlesLocked(userID, time.Now()) > 0
}

// handlesLocked counts every way user is currently in the call, across both
// membership dialects. Announcements are made on this number reaching or
// leaving zero, so that a user visible through both dialects at once — which is
// exactly what the bot itself looks like — is announced once, not twice.
func (w *callWatcher) handlesLocked(userID id.UserID, now time.Time) int {
	count := 0
	for stateKey := range w.present {
		if rtc.MembershipBelongsTo(stateKey, userID) {
			count++
		}
	}
	for handle, entry := range w.sticky {
		if handle.sender == userID && entry.joined && entry.expiresAt.After(now) {
			count++
		}
	}
	return count
}

// applySticky records a sticky membership event and reports what it means for
// the sender's presence overall.
func (w *callWatcher) applySticky(evt *event.Event, joined bool, expiresAt time.Time, stickyKey string, now time.Time) callChange {
	w.mu.Lock()
	defer w.mu.Unlock()

	handle := stickyHandle{sender: evt.Sender, stickyKey: stickyKey}
	score, eventID := rtc.StickyRank(evt)
	if existing, ok := w.sticky[handle]; ok && !supersedes(score, eventID, existing) {
		// An older or duplicate delivery: the same event arrives in both the
		// timeline and the sticky section, and either may land first.
		return callNoChange
	}

	before := w.handlesLocked(evt.Sender, now)
	w.sticky[handle] = &stickyEntry{joined: joined, expiresAt: expiresAt, score: score, eventID: eventID}
	after := w.handlesLocked(evt.Sender, now)

	if !w.stickyPrimed {
		return callNoChange
	}
	return transition(before, after)
}

// expireSticky drops memberships whose stickiness has run out and reports the
// users that leaves the call.
//
// Expiry is the only leave signal for a member who simply stops refreshing —
// a crashed client sends nothing at all — so this has to be swept rather than
// waited for.
func (w *callWatcher) expireSticky(now time.Time) []id.UserID {
	w.mu.Lock()
	defer w.mu.Unlock()

	var left []id.UserID
	announced := make(map[id.UserID]bool)
	for handle, entry := range w.sticky {
		if entry.expiresAt.After(now) {
			continue
		}
		delete(w.sticky, handle)
		// The entry counted towards its sender's presence right up to this
		// moment, so its removal is a leave exactly when nothing else is left.
		// Several of a user's memberships can lapse in the same sweep; that is
		// still one departure.
		if entry.joined && w.stickyPrimed && !announced[handle.sender] &&
			w.handlesLocked(handle.sender, now) == 0 {
			announced[handle.sender] = true
			left = append(left, handle.sender)
		}
	}
	return left
}

// stickyEventsOf lists the events behind a user's live sticky memberships.
//
// Event IDs, not sticky keys, because the only way to cancel someone else's
// entry is to redact the event: the ephemeral map is keyed by sender, so a
// leave published by the bot would create an entry of its own rather than
// removing theirs.
func (w *callWatcher) stickyEventsOf(userID id.UserID, now time.Time) []id.EventID {
	w.mu.Lock()
	defer w.mu.Unlock()

	var events []id.EventID
	for handle, entry := range w.sticky {
		if handle.sender == userID && entry.joined && entry.expiresAt.After(now) && entry.eventID != "" {
			events = append(events, id.EventID(entry.eventID))
		}
	}
	slices.Sort(events)
	return events
}

// forgetSticky drops a user's live sticky memberships from the map, for when
// the bot has just redacted them and should not wait out their stickiness.
func (w *callWatcher) forgetSticky(userID id.UserID) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for handle := range w.sticky {
		if handle.sender == userID {
			delete(w.sticky, handle)
		}
	}
}

// supersedes applies the MSC4354 tie-break for two events sharing a sticky key:
// the one that expires last wins, with the higher event ID breaking a draw.
func supersedes(score int64, eventID string, existing *stickyEntry) bool {
	if score != existing.score {
		return score > existing.score
	}
	return eventID > existing.eventID
}

// transition turns a before/after handle count into a callChange.
func transition(before, after int) callChange {
	switch {
	case before == 0 && after > 0:
		return callJoined
	case before > 0 && after == 0:
		return callLeft
	default:
		return callNoChange
	}
}

// prime marks the initial state as seen, so subsequent joins are announced.
func (w *callWatcher) prime() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.primed = true
}

// primeSticky is the sticky counterpart of prime. It is called after the first
// /sync rather than before syncing: sticky memberships are not room state, so
// there is nothing to read them from ahead of time.
func (w *callWatcher) primeSticky() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stickyPrimed = true
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
	change := b.calls.applyLegacy(evt.Sender, *evt.StateKey, joined)
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
		b.client.Log.Info().Str("user_id", evt.Sender.String()).Msg("left the call")
		b.announcePresence(ctx, evt.Sender, "left")
	case callNoChange:
		// Someone in the call from two devices who closes one has not left, and
		// neither has someone whose other dialect still says they are there.
		if !joined && b.calls.stillPresent(evt.Sender) {
			b.client.Log.Info().
				Str("user_id", evt.Sender.String()).
				Msg("left the call on one membership, still present on another")
		}
	}
}

// handleStickyMember is the sync handler for MSC4143 membership events.
//
// These arrive in the msc4354_sticky section of /sync as well as in the
// timeline, so the same event can be delivered more than once; the tie-break in
// applySticky makes that harmless.
func (b *Bot) handleStickyMember(ctx context.Context, evt *event.Event) {
	if evt.RoomID != b.roomID || evt.Sender == b.client.UserID {
		return
	}
	// Deliberately no isHistorical check. Sticky events are a replay of live
	// state by design — a membership sent ten minutes ago and still refreshing
	// is current — so the timestamp guard used for messages would discard all
	// of them. Stickiness expiry is the equivalent guard here.
	content, err := rtc.ParseStickyMember(evt)
	if err != nil {
		b.client.Log.Debug().Err(err).Str("event_id", evt.ID.String()).Msg("ignoring unparseable sticky membership")
		return
	}

	now := time.Now()
	expiresAt := rtc.StickyExpiry(evt, now)
	if expiresAt.IsZero() || !expiresAt.After(now) {
		return
	}

	joined := content.IsJoined()
	change := b.calls.applySticky(evt, joined, expiresAt, content.StickyKey, now)
	b.client.Log.Debug().
		Str("sticky_key", content.StickyKey).
		Str("sender", evt.Sender.String()).
		Bool("joined", joined).
		Int("change", int(change)).
		Msg("sticky call membership change")

	switch change {
	case callJoined:
		b.client.Log.Info().Str("user_id", evt.Sender.String()).Msg("joined the call")
		b.announcePresence(ctx, evt.Sender, "joined")
	case callLeft:
		b.client.Log.Info().Str("user_id", evt.Sender.String()).Msg("left the call")
		b.announcePresence(ctx, evt.Sender, "left")
	}
}

// sweepStickyMemberships announces the members whose stickiness has lapsed.
func (b *Bot) sweepStickyMemberships(ctx context.Context) {
	for _, user := range b.calls.expireSticky(time.Now()) {
		b.client.Log.Info().Str("user_id", user.String()).Msg("call membership expired")
		b.announcePresence(ctx, user, "left")
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

// stickyMemberEventType is the MSC4143 membership event, the message-event
// dialect of the same thing.
var stickyMemberEventType = rtc.StickyMemberEventType
