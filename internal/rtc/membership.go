package rtc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// CallMemberEventType is the state event other MatrixRTC clients (Element Call)
// read to discover who is in a call. This is the "session" form of MSC4143;
// newer sticky-event membership (MSC4354) is not implemented.
//
// The class must be set explicitly. event.NewEventType guesses it from a list
// of known types and returns UnknownEventType for anything unstable, while
// mautrix stamps StateEventType onto incoming state events — and both the sync
// listener map and the room state map are keyed by type *and* class. A type
// built with NewEventType therefore matches nothing: handlers never fire and
// state lookups come back empty, with no error to show for it.
var CallMemberEventType = event.Type{
	Type:  "org.matrix.msc3401.call.member",
	Class: event.StateEventType,
}

const (
	// applicationCall marks the session as a call rather than some other RTC app.
	applicationCall = "m.call"
	// scopeRoom is the single room-wide call, as opposed to breakout sessions.
	scopeRoom = "m.room"

	// membershipExpiry is how long a membership stays valid without an update.
	// It exists only to clean up after failed delayed events.
	membershipExpiry = 4 * time.Hour
	// delayedLeaveTimeout is how long the homeserver waits after our last
	// keepalive before publishing our leave event.
	delayedLeaveTimeout = 8 * time.Second
	// delayedLeaveRefresh must be comfortably shorter than the timeout.
	delayedLeaveRefresh = 5 * time.Second
)

// SessionMembership is the content of a session-style m.call.member event.
type SessionMembership struct {
	Application   string         `json:"application"`
	CallID        string         `json:"call_id"`
	DeviceID      string         `json:"device_id"`
	FocusActive   FocusSelection `json:"focus_active"`
	FociPreferred []Transport    `json:"foci_preferred"`
	CreatedTS     int64          `json:"created_ts,omitempty"`
	Scope         string         `json:"scope,omitempty"`
	Expires       int64          `json:"expires,omitempty"`
	// MembershipID is the identity used on the media backend. It must match the
	// LiveKit identity the JWT service derives, i.e. "<user_id>:<device_id>".
	MembershipID string `json:"membershipID,omitempty"`
}

// Membership publishes and maintains the bot's call membership state event.
type Membership struct {
	client   *mautrix.Client
	roomID   id.RoomID
	userID   id.UserID
	deviceID string
	stateKey string

	mu       sync.Mutex
	delayID  id.DelayID
	joined   bool
	stopKeep chan struct{}
	keepDone chan struct{}
}

// NewMembership builds a membership manager for the bot's own identity.
func NewMembership(client *mautrix.Client, roomID id.RoomID, userID id.UserID, deviceID string) *Membership {
	return &Membership{
		client:   client,
		roomID:   roomID,
		userID:   userID,
		deviceID: deviceID,
		// Element Call keys memberships by "_<user>_<device>". The leading
		// underscore keeps the key out of the "only its own user may set it"
		// namespace, so it is accepted in rooms of any version.
		stateKey: fmt.Sprintf("_%s_%s", userID, deviceID),
	}
}

// StateKey is the state key the bot's membership lives under.
func (m *Membership) StateKey() string { return m.stateKey }

// MembershipBelongsTo reports whether a membership state key is one of user's.
//
// Three key formats are in the wild: Element Call's "_<user>_<device>", the
// same without the leading underscore, and the original MSC3401 form of a bare
// user ID. Matching on the separator rather than a substring keeps "@bob:x"
// from claiming "@bobby:x"'s membership.
func MembershipBelongsTo(stateKey string, user id.UserID) bool {
	owner := user.String()
	return stateKey == owner ||
		strings.HasPrefix(stateKey, "_"+owner+"_") ||
		strings.HasPrefix(stateKey, owner+"_")
}

// MembershipID is the LiveKit identity implied by this membership.
func (m *Membership) MembershipID() string {
	return fmt.Sprintf("%s:%s", m.userID, m.deviceID)
}

// ActiveTransport returns the LiveKit transport the call is already using, by
// the "oldest membership wins" rule every MatrixRTC client follows. It returns
// nil when nobody else is in the call, in which case the caller proposes its
// own transport.
func (m *Membership) ActiveTransport(ctx context.Context) (*Transport, error) {
	fullState, err := m.client.State(ctx, m.roomID)
	if err != nil {
		return nil, fmt.Errorf("fetch room state: %w", err)
	}
	members := fullState[CallMemberEventType]

	var oldest *event.Event
	for stateKey, evt := range members {
		if stateKey == m.stateKey || evt == nil {
			continue
		}
		var content SessionMembership
		if err := parseContent(evt, &content); err != nil {
			continue
		}
		if content.Application != applicationCall || len(content.FociPreferred) == 0 {
			continue
		}
		if isExpired(evt, &content) {
			continue
		}
		if oldest == nil || createdTS(evt, &content) < createdTS(oldest, nil) {
			oldest = evt
		}
	}
	if oldest == nil {
		return nil, nil
	}
	var content SessionMembership
	if err := parseContent(oldest, &content); err != nil {
		return nil, err
	}
	for _, transport := range content.FociPreferred {
		if transport.Type == TransportTypeLiveKit && transport.LiveKitServiceURL != "" {
			t := transport
			return &t, nil
		}
	}
	return nil, nil
}

// Join publishes the bot's membership and starts the delayed-leave keepalive.
// transport is the focus the bot proposes (or echoes back, if it joined an
// existing call).
func (m *Membership) Join(ctx context.Context, transport Transport) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.joined {
		return nil
	}

	// Arm the delayed leave *before* announcing ourselves, so a crash between
	// the two cannot leave a permanent ghost participant in the call.
	if err := m.armDelayedLeaveLocked(ctx); err != nil {
		// Not fatal: homeservers without MSC4140 still work, they just rely on
		// the expiry field and our shutdown handler.
		m.client.Log.Warn().Err(err).Msg("delayed leave event unavailable; relying on expiry and clean shutdown")
	}

	now := time.Now().UnixMilli()
	content := SessionMembership{
		Application:   applicationCall,
		CallID:        "",
		DeviceID:      m.deviceID,
		FocusActive:   FocusSelection{Type: TransportTypeLiveKit, FocusSelection: "oldest_membership"},
		FociPreferred: []Transport{transport},
		CreatedTS:     now,
		Scope:         scopeRoom,
		Expires:       membershipExpiry.Milliseconds(),
		MembershipID:  m.MembershipID(),
	}
	if _, err := m.client.SendStateEvent(ctx, m.roomID, CallMemberEventType, m.stateKey, &content); err != nil {
		return fmt.Errorf("send call membership: %w", err)
	}
	m.joined = true

	if m.delayID != "" {
		m.stopKeep = make(chan struct{})
		m.keepDone = make(chan struct{})
		go m.keepalive(m.stopKeep, m.keepDone, m.delayID)
	}
	return nil
}

// Leave retracts the membership and cancels the pending delayed leave.
func (m *Membership) Leave(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.joined {
		return nil
	}
	if m.stopKeep != nil {
		close(m.stopKeep)
		<-m.keepDone
		m.stopKeep, m.keepDone = nil, nil
	}
	if m.delayID != "" {
		// Let the scheduled leave fire now rather than cancelling it: that is
		// exactly the event we want published.
		_, err := m.client.UpdateDelayedEvent(ctx, &mautrix.ReqUpdateDelayedEvent{
			DelayID: m.delayID,
			Action:  event.DelayActionSend,
		})
		m.delayID = ""
		m.joined = false
		if err == nil {
			return nil
		}
		m.client.Log.Warn().Err(err).Msg("could not trigger delayed leave; sending empty membership directly")
	}
	m.joined = false
	if _, err := m.client.SendStateEvent(ctx, m.roomID, CallMemberEventType, m.stateKey, struct{}{}); err != nil {
		return fmt.Errorf("retract call membership: %w", err)
	}
	return nil
}

// armDelayedLeaveLocked schedules an empty membership event that the homeserver
// publishes if we stop refreshing it. Caller must hold m.mu.
func (m *Membership) armDelayedLeaveLocked(ctx context.Context) error {
	resp, err := m.client.SendStateEvent(ctx, m.roomID, CallMemberEventType, m.stateKey, struct{}{}, mautrix.ReqSendEvent{
		UnstableDelay: delayedLeaveTimeout,
	})
	if err != nil {
		return err
	}
	if resp.UnstableDelayID == "" {
		return fmt.Errorf("homeserver did not return a delay id")
	}
	m.delayID = resp.UnstableDelayID
	return nil
}

// keepalive restarts the delayed leave timer until the bot shuts down.
func (m *Membership) keepalive(stop <-chan struct{}, done chan<- struct{}, delayID id.DelayID) {
	defer close(done)
	ticker := time.NewTicker(delayedLeaveRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), delayedLeaveRefresh)
			_, err := m.client.UpdateDelayedEvent(ctx, &mautrix.ReqUpdateDelayedEvent{
				DelayID: delayID,
				Action:  event.DelayActionRestart,
			})
			cancel()
			if err != nil {
				m.client.Log.Warn().Err(err).Msg("failed to restart delayed leave event")
			}
		}
	}
}

func parseContent(evt *event.Event, out *SessionMembership) error {
	if evt == nil {
		return fmt.Errorf("nil event")
	}
	raw, err := evt.Content.MarshalJSON()
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(raw)) == "{}" {
		return fmt.Errorf("empty membership")
	}
	return json.Unmarshal(raw, out)
}

func createdTS(evt *event.Event, content *SessionMembership) int64 {
	if content != nil && content.CreatedTS > 0 {
		return content.CreatedTS
	}
	return evt.Timestamp
}

func isExpired(evt *event.Event, content *SessionMembership) bool {
	if content.Expires <= 0 {
		return false
	}
	return time.Now().UnixMilli() > createdTS(evt, content)+content.Expires
}
