package rtc

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// The mirror image of the class trap guarded in membership_test.go: sticky
// membership arrives without a state key, so mautrix stamps MessageEventType on
// it. A type built any other way matches nothing and the handler never fires.
func TestStickyMemberEventTypeIsAMessageEvent(t *testing.T) {
	if got := StickyMemberEventType.Class; got != event.MessageEventType {
		t.Errorf("StickyMemberEventType.Class = %v; want MessageEventType", got)
	}
	if got := StickyMemberEventType.Type; got != "org.matrix.msc4143.rtc.member" {
		t.Errorf("StickyMemberEventType.Type = %q; unexpected", got)
	}
	if guessed := event.NewEventType("org.matrix.msc4143.rtc.member"); guessed == StickyMemberEventType {
		t.Skip("mautrix now recognises this type; the explicit class is redundant")
	}
}

func newTestStickyMembership(t *testing.T) *StickyMembership {
	t.Helper()
	m, err := NewStickyMembership(nil, "!room:example.org", DefaultSlotID, DefaultStickyDuration)
	if err != nil {
		t.Fatalf("NewStickyMembership() = %v", err)
	}
	return m
}

// The ephemeral map is keyed by the sticky key, and MSC4143 requires it to be
// the member ID. Diverge and every refresh looks like a second participant.
func TestJoinContentShape(t *testing.T) {
	m := newTestStickyMembership(t)
	content := m.joinContent()

	if content.StickyKey != content.Member.ID {
		t.Errorf("sticky key %q != member id %q", content.StickyKey, content.Member.ID)
	}
	if content.Member.ID != m.MemberID() {
		t.Errorf("member id %q != MemberID() %q", content.Member.ID, m.MemberID())
	}
	if content.Member.Membership != "join" {
		t.Errorf("membership = %q; want join", content.Member.Membership)
	}
	if content.SlotID != DefaultSlotID {
		t.Errorf("slot_id = %q; want %q", content.SlotID, DefaultSlotID)
	}
	if content.Application == nil || content.Application.Type != "m.call" {
		t.Errorf("application = %+v; want m.call", content.Application)
	}
	if content.Transports == nil || len(content.Transports.Published) != 1 ||
		content.Transports.Published[0].Type != TransportTypeLiveKit {
		t.Errorf("published transports = %+v; want one livekit entry", content.Transports)
	}
	// The transport carries no URL: under MSC4195 the token, and with it the
	// SFU address, comes from each member's own homeserver.
	raw, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["msc4354_sticky_key"]; !ok {
		t.Errorf("content is missing the unstable sticky key field: %s", raw)
	}
	if got := string(raw); strings.Contains(got, "livekit_service_url") {
		t.Errorf("published transport leaked a service URL: %s", got)
	}
}

func TestLeaveContentShape(t *testing.T) {
	m := newTestStickyMembership(t)
	content := m.leaveContent(leaveReasonNormal)

	if content.Member.Membership != "leave" {
		t.Errorf("membership = %q; want leave", content.Member.Membership)
	}
	if content.StickyKey != m.MemberID() {
		t.Errorf("sticky key %q != member id %q", content.StickyKey, m.MemberID())
	}
	if content.LeaveReason == nil || content.LeaveReason.Code != leaveReasonNormal {
		t.Errorf("leave_reason = %+v; want code %q", content.LeaveReason, leaveReasonNormal)
	}
	// A leave carries no transports: there is nothing left to connect to.
	if content.Transports != nil {
		t.Errorf("leave advertised transports: %+v", content.Transports)
	}
}

// MSC4143: the member ID must be unique for each join, so a client that leaves
// and rejoins is not mistaken for the one that was already there.
func TestMemberIDIsFreshPerMembership(t *testing.T) {
	seen := map[string]bool{}
	for range 8 {
		m := newTestStickyMembership(t)
		if seen[m.MemberID()] {
			t.Fatalf("member id %q reused", m.MemberID())
		}
		seen[m.MemberID()] = true
	}
}

// The refresh has to land well before the membership lapses, or the bot
// flickers in and out of the participant list.
func TestRefreshIntervalLeavesHeadroom(t *testing.T) {
	m := newTestStickyMembership(t)
	if got := m.refreshInterval(); got >= m.duration/2 {
		t.Errorf("refreshInterval() = %s; want comfortably under half of %s", got, m.duration)
	}
}

func TestStickyDurationIsCapped(t *testing.T) {
	m, err := NewStickyMembership(nil, "!room:example.org", DefaultSlotID, 6*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if m.duration != event.MaxStickyDuration {
		t.Errorf("duration = %s; want the MSC4354 cap of %s", m.duration, event.MaxStickyDuration)
	}
}

func stickyEvent(t *testing.T, content any, duration time.Duration, ts int64, eventID string) *event.Event {
	t.Helper()
	raw, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	evt := &event.Event{
		ID:        id.EventID(eventID),
		Type:      StickyMemberEventType,
		Timestamp: ts,
		Sticky:    &event.Sticky{},
	}
	evt.Sticky.Duration.Duration = duration
	evt.Content.VeryRaw = raw
	return evt
}

func TestParseStickyMember(t *testing.T) {
	m := newTestStickyMembership(t)
	evt := stickyEvent(t, m.joinContent(), time.Minute, 1000, "$a")

	content, err := ParseStickyMember(evt)
	if err != nil {
		t.Fatalf("ParseStickyMember() = %v", err)
	}
	if !content.IsJoined() {
		t.Error("IsJoined() = false for a join event")
	}
	if content.StickyKey != m.MemberID() {
		t.Errorf("sticky key = %q; want %q", content.StickyKey, m.MemberID())
	}

	// A membership without a sticky key cannot be placed in the ephemeral map,
	// so it is not a membership this bot can act on.
	if _, err := ParseStickyMember(stickyEvent(t, map[string]any{"slot_id": "x"}, time.Minute, 1, "$b")); err == nil {
		t.Error("ParseStickyMember() accepted content with no sticky key")
	}
}

// The homeserver's own countdown is measured against the clock that set the
// expiry, so it beats recomputing from timestamps that may be skewed.
func TestStickyExpiryPrefersServerTTL(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	evt := stickyEvent(t, map[string]any{"msc4354_sticky_key": "k"}, 10*time.Minute,
		now.Add(-time.Hour).UnixMilli(), "$a")
	evt.Unsigned.StickyDurationTTL.Duration = 4 * time.Minute

	if got := StickyExpiry(evt, now); !got.Equal(now.Add(4 * time.Minute)) {
		t.Errorf("StickyExpiry() = %s; want %s", got, now.Add(4*time.Minute))
	}

	// Without a TTL it falls back to the timestamp, which here is already past.
	evt.Unsigned.StickyDurationTTL.Duration = 0
	if got := StickyExpiry(evt, now); !got.Before(now) {
		t.Errorf("StickyExpiry() = %s; want an expiry in the past", got)
	}

	// An event that is not sticky at all has no expiry to speak of.
	plain := stickyEvent(t, map[string]any{"msc4354_sticky_key": "k"}, 0, now.UnixMilli(), "$c")
	if got := StickyExpiry(plain, now); !got.IsZero() {
		t.Errorf("StickyExpiry() = %s for a non-sticky event; want zero", got)
	}
}

func TestStickyRankIsLastToExpire(t *testing.T) {
	early := stickyEvent(t, map[string]any{"msc4354_sticky_key": "k"}, time.Minute, 1000, "$a")
	late := stickyEvent(t, map[string]any{"msc4354_sticky_key": "k"}, 10*time.Minute, 1000, "$b")

	earlyScore, _ := StickyRank(early)
	lateScore, _ := StickyRank(late)
	if lateScore <= earlyScore {
		t.Errorf("StickyRank: %d (10m) should beat %d (1m)", lateScore, earlyScore)
	}
}
