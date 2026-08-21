package rtc

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// StickyMemberEventType is the MSC4143 membership event. Unlike the session-style
// membership it is a *message* event, not state: it carries an MSC4354 sticky
// duration and expires on its own.
//
// The class matters as much here as it does for CallMemberEventType, just in the
// other direction: mautrix stamps MessageEventType onto anything arriving without
// a state key — the msc4354_sticky sync section included — and listeners are keyed
// by type and class together.
var StickyMemberEventType = event.Type{
	Type:  "org.matrix.msc4143.rtc.member",
	Class: event.MessageEventType,
}

// StickyKeyField is the content key of the MSC4354 ephemeral map. Clients track
// entries by (room_id, sender, type, sticky_key); ours always holds member.id.
const StickyKeyField = "msc4354_sticky_key"

const (
	// DefaultStickyDuration is how long a published membership stays valid
	// without a refresh. MSC4354 caps stickiness at an hour and advises against
	// going below five minutes.
	DefaultStickyDuration = 10 * time.Minute

	// membershipJoin and membershipLeave are the two member.membership values.
	membershipJoin  = "join"
	membershipLeave = "leave"

	// leaveReasonNormal is a member hanging up rather than timing out.
	leaveReasonNormal = "leave"
	// leaveReasonDelayed is the homeserver publishing our leave for us.
	leaveReasonDelayed = "delayed_leave"
)

// StickyMemberInfo identifies one membership of one user.
type StickyMemberInfo struct {
	// ID distinguishes several memberships of the same user and device, and
	// must be fresh for every join.
	ID string `json:"id"`
	// Membership is "join" or "leave".
	Membership string `json:"membership"`
}

// StickyTransports describes how to exchange media with a member.
type StickyTransports struct {
	// Published carries no URLs: under MSC4195 each member's own homeserver
	// issues the token, and with it the SFU address.
	Published    []RTCTransport `json:"published"`
	CanSubscribe []string       `json:"can_subscribe"`
}

// StickyLeaveReason explains why a member left.
type StickyLeaveReason struct {
	Code   string `json:"code"`
	Reason string `json:"reason,omitempty"`
}

// StickyMemberContent is the content of an m.rtc.member event.
type StickyMemberContent struct {
	SlotID      string             `json:"slot_id"`
	Member      StickyMemberInfo   `json:"member"`
	Application *SlotApplication   `json:"application,omitempty"`
	Transports  *StickyTransports  `json:"transports,omitempty"`
	LeaveReason *StickyLeaveReason `json:"leave_reason,omitempty"`
	// StickyKey must equal Member.ID.
	StickyKey string `json:"msc4354_sticky_key"`
}

// StickyMembership publishes and refreshes the bot's sticky m.rtc.member event.
//
// It is the sticky-stack counterpart of Membership and deliberately mirrors its
// shape, but the two are independent: the bot runs both at once so clients on
// either side of the MatrixRTC transition can see it.
type StickyMembership struct {
	client   *mautrix.Client
	roomID   id.RoomID
	slotID   string
	memberID string
	duration time.Duration

	mu       sync.Mutex
	delayID  id.DelayID
	joined   bool
	keeper   *delayKeeper
	stopRe   chan struct{}
	reDone   chan struct{}
	lastSend time.Time
}

// NewStickyMembership builds a membership manager with a fresh member ID.
//
// The ID must be unique for every join of the same user — a client that leaves
// and rejoins uses a new one — so it is generated here rather than derived from
// anything stable.
func NewStickyMembership(client *mautrix.Client, roomID id.RoomID, slotID string, duration time.Duration) (*StickyMembership, error) {
	memberID, err := newMemberID()
	if err != nil {
		return nil, err
	}
	if duration <= 0 {
		duration = DefaultStickyDuration
	}
	if duration > event.MaxStickyDuration {
		duration = event.MaxStickyDuration
	}
	return &StickyMembership{
		client:   client,
		roomID:   roomID,
		slotID:   slotID,
		memberID: memberID,
		duration: duration,
	}, nil
}

// MemberID is the member.id this membership publishes under. The LiveKit
// identity is derived from it, so the token request must use the same value.
func (m *StickyMembership) MemberID() string { return m.memberID }

// refreshInterval is how often the membership is re-published. It sits well
// ahead of expiry: MSC4143 asks clients to refresh early so the membership
// never flickers between expired and renewed.
func (m *StickyMembership) refreshInterval() time.Duration {
	return max(m.duration*2/5, time.Second)
}

// Join publishes the membership and starts refreshing it.
func (m *StickyMembership) Join(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.joined {
		return nil
	}

	// Arm the delayed leave first, for the same reason the session stack does:
	// a crash between arming and joining must not leave a ghost participant.
	if err := m.armDelayedLeaveLocked(ctx); err != nil {
		m.client.Log.Warn().Err(err).
			Msg("delayed leave unavailable for sticky membership; relying on stickiness expiry and clean shutdown")
	}

	if err := m.sendLocked(ctx, m.joinContent()); err != nil {
		return fmt.Errorf("send sticky call membership: %w", err)
	}
	m.joined = true

	if m.delayID != "" {
		m.keeper = startDelayKeeper(m.client, "sticky", m.delayID, delayedLeaveRefresh, m.recover)
	}
	m.stopRe, m.reDone = make(chan struct{}), make(chan struct{})
	go m.refresh(m.stopRe, m.reDone)
	return nil
}

// Leave retracts the membership and cancels the pending delayed leave.
func (m *StickyMembership) Leave(ctx context.Context) error {
	m.mu.Lock()
	if !m.joined {
		m.mu.Unlock()
		return nil
	}
	m.joined = false
	keeper, stopRe, reDone := m.keeper, m.stopRe, m.reDone
	m.keeper, m.stopRe, m.reDone = nil, nil, nil
	m.mu.Unlock()

	// Stop the background loops without holding the lock. Both of them take it,
	// and waiting for them here while holding it would deadlock.
	if stopRe != nil {
		close(stopRe)
		<-reDone
	}
	keeper.Stop()

	m.mu.Lock()
	defer m.mu.Unlock()
	if delayID := latestDelayID(keeper, m.delayID); delayID != "" {
		// Fire the scheduled leave rather than cancelling it: that is exactly
		// the event we want published.
		_, err := m.client.UpdateDelayedEvent(ctx, &mautrix.ReqUpdateDelayedEvent{
			DelayID: delayID,
			Action:  event.DelayActionSend,
		})
		m.delayID = ""
		if err == nil {
			return nil
		}
		m.client.Log.Warn().Err(err).Msg("could not trigger sticky delayed leave; sending leave directly")
	}
	if err := m.sendLocked(ctx, m.leaveContent(leaveReasonNormal)); err != nil {
		return fmt.Errorf("retract sticky call membership: %w", err)
	}
	return nil
}

// recover re-publishes the membership and arms a new delayed leave, after the
// old one went missing.
//
// The refresh loop would eventually put the membership back on its own, but not
// for minutes; a delayed leave that fired by accident has already told every
// client in the room that the bot left, and it is still streaming to them. This
// closes that window in seconds instead.
func (m *StickyMembership) recover(ctx context.Context) (id.DelayID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.joined {
		return "", fmt.Errorf("not joined")
	}
	if err := m.armDelayedLeaveLocked(ctx); err != nil {
		return "", err
	}
	if err := m.sendLocked(ctx, m.joinContent()); err != nil {
		return "", fmt.Errorf("re-send sticky call membership: %w", err)
	}
	return m.delayID, nil
}

// joinContent is the membership the bot publishes while it is in the call.
func (m *StickyMembership) joinContent() *StickyMemberContent {
	return &StickyMemberContent{
		SlotID:      m.slotID,
		Member:      StickyMemberInfo{ID: m.memberID, Membership: membershipJoin},
		Application: &SlotApplication{Type: applicationCall},
		Transports: &StickyTransports{
			Published:    []RTCTransport{{Type: TransportTypeLiveKit}},
			CanSubscribe: []string{TransportTypeLiveKit},
		},
		StickyKey: m.memberID,
	}
}

// leaveContent is the membership that takes the bot out of the call.
func (m *StickyMembership) leaveContent(code string) *StickyMemberContent {
	return &StickyMemberContent{
		SlotID:      m.slotID,
		Member:      StickyMemberInfo{ID: m.memberID, Membership: membershipLeave},
		LeaveReason: &StickyLeaveReason{Code: code},
		StickyKey:   m.memberID,
	}
}

// refresh re-publishes the membership before its stickiness runs out.
func (m *StickyMembership) refresh(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(m.refreshInterval())
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), m.refreshInterval())
			m.mu.Lock()
			var err error
			if m.joined {
				err = m.sendLocked(ctx, m.joinContent())
			}
			m.mu.Unlock()
			cancel()
			if err != nil {
				m.client.Log.Warn().Err(err).Msg("failed to refresh sticky call membership")
			}
		}
	}
}

// sendLocked publishes one membership event. Caller must hold m.mu.
//
// Two events sharing a sticky key are tie-broken on origin_server_ts first, so
// sending two within the same millisecond risks the older one winning and the
// update being silently undone. A millisecond of spacing rules that out.
func (m *StickyMembership) sendLocked(ctx context.Context, content *StickyMemberContent) error {
	if since := time.Since(m.lastSend); !m.lastSend.IsZero() && since < time.Millisecond {
		time.Sleep(time.Millisecond - since)
	}
	_, err := m.client.SendMessageEvent(ctx, m.roomID, StickyMemberEventType, content, mautrix.ReqSendEvent{
		UnstableStickyDuration: m.duration,
	})
	m.lastSend = time.Now()
	return err
}

// armDelayedLeaveLocked schedules the leave event the homeserver publishes if we
// stop refreshing it. Caller must hold m.mu.
func (m *StickyMembership) armDelayedLeaveLocked(ctx context.Context) error {
	resp, err := m.client.SendMessageEvent(ctx, m.roomID, StickyMemberEventType,
		m.leaveContent(leaveReasonDelayed), mautrix.ReqSendEvent{
			UnstableDelay:          delayedLeaveTimeout,
			UnstableStickyDuration: m.duration,
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

// newMemberID returns a random identifier with enough entropy that the SFU
// cannot guess it: the pseudonymous LiveKit identity is a hash over the user
// ID, the device ID and this value, and the first two are hardly secret.
func newMemberID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate member id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// ParseStickyMember decodes an m.rtc.member event's content.
//
// The raw bytes are the reliable source: mautrix does not parse this unstable
// type, so the decoded content is empty either way.
func ParseStickyMember(evt *event.Event) (*StickyMemberContent, error) {
	if evt == nil {
		return nil, fmt.Errorf("nil event")
	}
	raw := evt.Content.VeryRaw
	if len(raw) == 0 {
		var err error
		if raw, err = evt.Content.MarshalJSON(); err != nil {
			return nil, err
		}
	}
	var content StickyMemberContent
	if err := json.Unmarshal(raw, &content); err != nil {
		return nil, err
	}
	if content.StickyKey == "" {
		return nil, fmt.Errorf("sticky membership without a sticky key")
	}
	return &content, nil
}

// IsJoined reports whether this membership puts its member in the call.
func (c *StickyMemberContent) IsJoined() bool {
	return c != nil && c.Member.Membership == membershipJoin
}

// StickyExpiry is when a sticky event stops being sticky, as seen from here.
//
// The homeserver's own countdown is preferred when it sent one: it is measured
// against the clock that decided the expiry, so using it removes the skew
// between that clock and ours. Otherwise the MSC4354 rule applies, taking the
// earlier of the origin and receive timestamps so a future-dated event cannot
// extend its own life.
func StickyExpiry(evt *event.Event, now time.Time) time.Time {
	duration := evt.Sticky.GetDuration()
	if duration == 0 {
		return time.Time{}
	}
	if ttl := evt.Unsigned.StickyDurationTTL.Duration; ttl > 0 {
		return now.Add(min(ttl, duration))
	}
	start := time.UnixMilli(evt.Timestamp)
	if received := evt.Mautrix.ReceivedAt; !received.IsZero() && received.Before(start) {
		start = received
	}
	return start.Add(duration)
}

// StickyRank orders two events sharing a sticky key: last to expire wins, with
// the event ID as a tie-break. It reports whether evt beats a stored entry with
// the given rank.
func StickyRank(evt *event.Event) (score int64, eventID string) {
	return evt.Timestamp + evt.Sticky.GetDuration().Milliseconds(), evt.ID.String()
}
