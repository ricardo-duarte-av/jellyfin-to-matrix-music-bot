package rtc

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/rs/zerolog"
)

// fakeMember is one dialect's membership, recording what it was told to publish.
type fakeMember struct {
	joins     int
	leaves    int
	transport Transport
	joinErr   error
	// dialect pairs the membership with a leg; empty means "leg".
	dialect string
	// connected is what the fan-out looked like at the moment this membership
	// was published, which is how the ordering is checked.
	connected string
	publisher *MultiPublisher
}

func (f *fakeMember) Dialect() string {
	if f.dialect == "" {
		return "leg"
	}
	return f.dialect
}

func (f *fakeMember) Join(_ context.Context, transport Transport) error {
	if f.joinErr != nil {
		return f.joinErr
	}
	f.joins++
	f.transport = transport
	if f.publisher != nil {
		f.connected = f.publisher.Identity()
	}
	return nil
}

func (f *fakeMember) Leave(context.Context) error {
	f.leaves++
	return nil
}

// idleSession is a session over one leg that is not connected yet.
func idleSession(t *testing.T, dial dialer, resolve func(context.Context) (string, error), members ...CallMember) (*Session, *MultiPublisher, *Focus) {
	t.Helper()
	log := zerolog.New(io.Discard)
	m := &MultiPublisher{log: log, done: make(chan struct{}), halt: make(chan struct{}), suspended: true}
	m.legs = []*leg{{name: "leg", dead: true, dial: dial}}
	focus := NewFocus("https://focus.example.org")
	return NewSession(log, focus, m, resolve, members...), m, focus
}

// A membership is a promise that the bot can be heard. Publishing one and then
// failing to connect advertises a participant that makes no sound, which is the
// exact state the rest of this package exists to avoid — so the connection is
// made first.
func TestEnterConnectsBeforePublishingMemberships(t *testing.T) {
	member := &fakeMember{}
	s, m, _ := idleSession(t, func(context.Context) (publisherLeg, error) {
		return &fakeLeg{identity: "bot:DEVICE"}, nil
	}, nil, member)
	member.publisher = m

	if err := s.Enter(context.Background()); err != nil {
		t.Fatalf("Enter() = %v", err)
	}
	if member.joins != 1 {
		t.Fatalf("published %d memberships; want 1", member.joins)
	}
	if member.connected != "leg=bot:DEVICE" {
		t.Errorf("the membership was published with the fan-out at %q; want the connection already up", member.connected)
	}
	if !s.Joined() {
		t.Error("Joined() = false after entering the call")
	}
}

// Failing to get onto the SFU must leave no trace in the room: a membership
// published here would be a participant nobody can hear, kept alive by the
// refresh loops until someone restarted the bot.
func TestEnterPublishesNothingWhenItCannotConnect(t *testing.T) {
	member := &fakeMember{}
	s, _, _ := idleSession(t, func(context.Context) (publisherLeg, error) {
		return nil, errors.New("livekit is down")
	}, nil, member)

	if err := s.Enter(context.Background()); err == nil {
		t.Fatal("Enter() = nil with nothing to connect to")
	}
	if member.joins != 0 {
		t.Errorf("published %d memberships without a connection; want none", member.joins)
	}
	if s.Joined() {
		t.Error("Joined() = true after failing to enter")
	}
}

// The bot leaves an empty call, so the next one is somebody else's: MatrixRTC
// puts everyone on the oldest membership's focus, and the bot has to follow the
// one the room agreed on rather than impose the one it started with.
func TestEnterFollowsTheFocusTheCallIsAlreadyOn(t *testing.T) {
	member := &fakeMember{}
	const theirs = "https://someone-elses-focus.example.org"
	s, _, focus := idleSession(t, func(context.Context) (publisherLeg, error) {
		return &fakeLeg{}, nil
	}, func(context.Context) (string, error) { return theirs, nil }, member)
	focus.SetAlias("ROOM-ALIAS")

	if err := s.Enter(context.Background()); err != nil {
		t.Fatalf("Enter() = %v", err)
	}
	if got := member.transport.LiveKitServiceURL; got != theirs {
		t.Errorf("published focus %q; want the one the call is on, %q", got, theirs)
	}
	if got := member.transport.LiveKitAlias; got != "ROOM-ALIAS" {
		t.Errorf("published alias %q; want the room the token was minted for", got)
	}
}

// Leaving retracts what the room reads and then drops what it costs.
func TestLeaveRetractsTheMembershipsAndDisconnects(t *testing.T) {
	member := &fakeMember{}
	connection := &fakeLeg{}
	s, m, _ := idleSession(t, func(context.Context) (publisherLeg, error) { return connection, nil }, nil, member)

	if err := s.Enter(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Leave(context.Background()); err != nil {
		t.Fatalf("Leave() = %v", err)
	}

	if member.leaves != 1 {
		t.Errorf("retracted %d memberships; want 1", member.leaves)
	}
	if _, _, _, closed := connection.counts(); !closed {
		t.Error("left the call still holding the SFU connection")
	}
	if s.Joined() {
		t.Error("Joined() = true after leaving")
	}
	m.mu.Lock()
	suspended := m.suspended
	m.mu.Unlock()
	if !suspended {
		t.Error("the publisher was left live after leaving the call")
	}
}

// Both directions are driven by membership changes, which arrive in bursts and
// from several goroutines, so doing either twice has to be free.
func TestEnterAndLeaveAreIdempotent(t *testing.T) {
	member := &fakeMember{}
	dials := 0
	s, _, _ := idleSession(t, func(context.Context) (publisherLeg, error) {
		dials++
		return &fakeLeg{}, nil
	}, nil, member)

	ctx := context.Background()
	if err := s.Leave(ctx); err != nil {
		t.Fatalf("Leave() before entering = %v", err)
	}
	if err := s.Enter(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Enter(ctx); err != nil {
		t.Fatalf("second Enter() = %v", err)
	}
	if dials != 1 || member.joins != 1 {
		t.Errorf("entering twice dialled %d times and published %d memberships; want 1 each", dials, member.joins)
	}
	if err := s.Leave(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Leave(ctx); err != nil {
		t.Fatalf("second Leave() = %v", err)
	}
	if member.leaves != 1 {
		t.Errorf("retracted %d memberships; want 1", member.leaves)
	}
}

// twoDialectSession is a session over a legacy and a sticky leg, neither
// connected yet, choosing its dialects from whatever *want holds.
func twoDialectSession(t *testing.T, want *[]string) (s *Session, legacy, sticky *fakeMember, conns map[string][]*fakeLeg) {
	t.Helper()
	log := zerolog.New(io.Discard)
	conns = map[string][]*fakeLeg{}
	dialFor := func(name string) dialer {
		return func(context.Context) (publisherLeg, error) {
			conn := &fakeLeg{identity: name}
			conns[name] = append(conns[name], conn)
			return conn, nil
		}
	}
	m := &MultiPublisher{log: log, done: make(chan struct{}), halt: make(chan struct{}), suspended: true}
	m.legs = []*leg{
		{name: DialectLegacy, dead: true, dial: dialFor(DialectLegacy)},
		{name: DialectSticky, dead: true, dial: dialFor(DialectSticky)},
	}
	legacy = &fakeMember{dialect: DialectLegacy}
	sticky = &fakeMember{dialect: DialectSticky}
	s = NewSession(log, NewFocus("https://focus.example.org"), m, nil, legacy, sticky)
	s.ChooseDialects(func() []string { return *want })
	return s, legacy, sticky, conns
}

// A client reading both dialects shows a bot on both as two participants, so
// the session joins only on the dialects it is told the call needs — and does
// not even connect the other.
func TestEnterJoinsOnlyTheChosenDialects(t *testing.T) {
	want := []string{DialectSticky}
	s, legacy, sticky, conns := twoDialectSession(t, &want)

	if err := s.Enter(context.Background()); err != nil {
		t.Fatalf("Enter() = %v", err)
	}
	if legacy.joins != 0 || sticky.joins != 1 {
		t.Errorf("joins legacy=%d sticky=%d; want 0 and 1", legacy.joins, sticky.joins)
	}
	if len(conns[DialectLegacy]) != 0 {
		t.Error("connected the legacy leg for a dialect nobody needs")
	}
	if len(conns[DialectSticky]) != 1 {
		t.Errorf("dialled the sticky leg %d times; want 1", len(conns[DialectSticky]))
	}
}

// With nobody to go by the bot cannot know what the call reads, and being seen
// twice beats not being seen.
func TestEnterJoinsEveryDialectWhenNoneIsChosen(t *testing.T) {
	var want []string
	s, legacy, sticky, _ := twoDialectSession(t, &want)

	if err := s.Enter(context.Background()); err != nil {
		t.Fatalf("Enter() = %v", err)
	}
	if legacy.joins != 1 || sticky.joins != 1 {
		t.Errorf("joins legacy=%d sticky=%d; want both", legacy.joins, sticky.joins)
	}
}

// Moving between dialects joins the new one before leaving the old, so no
// client loses the bot in between, and drops the old connection.
func TestReconcileSwitchesDialects(t *testing.T) {
	want := []string{DialectLegacy}
	s, legacy, sticky, conns := twoDialectSession(t, &want)
	ctx := context.Background()
	if err := s.Enter(ctx); err != nil {
		t.Fatal(err)
	}

	want = []string{DialectSticky}
	if err := s.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() = %v", err)
	}
	if sticky.joins != 1 || legacy.leaves != 1 {
		t.Errorf("sticky joins=%d legacy leaves=%d; want 1 and 1", sticky.joins, legacy.leaves)
	}
	if _, _, _, closed := conns[DialectLegacy][0].counts(); !closed {
		t.Error("kept the legacy connection after leaving that dialect")
	}
	if len(conns[DialectSticky]) != 1 {
		t.Fatalf("dialled the sticky leg %d times; want 1", len(conns[DialectSticky]))
	}
	if _, _, _, closed := conns[DialectSticky][0].counts(); closed {
		t.Error("closed the connection of the dialect it switched to")
	}

	// Nothing changed, so nothing happens.
	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if sticky.joins != 1 || legacy.joins != 1 {
		t.Errorf("a no-op Reconcile rejoined: legacy=%d sticky=%d", legacy.joins, sticky.joins)
	}

	// Leaving the call retracts only the dialect the bot is on.
	if err := s.Leave(ctx); err != nil {
		t.Fatal(err)
	}
	if legacy.leaves != 1 || sticky.leaves != 1 {
		t.Errorf("leaves legacy=%d sticky=%d; want 1 each", legacy.leaves, sticky.leaves)
	}
}

// A dialect that will not take the bot keeps it where it was: dropping the old
// one first would leave its clients with nobody.
func TestReconcileKeepsTheOldDialectWhenTheNewOneFails(t *testing.T) {
	want := []string{DialectLegacy}
	s, legacy, sticky, _ := twoDialectSession(t, &want)
	ctx := context.Background()
	if err := s.Enter(ctx); err != nil {
		t.Fatal(err)
	}

	sticky.joinErr = errors.New("slot closed")
	want = []string{DialectSticky}
	if err := s.Reconcile(ctx); err == nil {
		t.Fatal("Reconcile() = nil with the new dialect refusing the bot")
	}
	if legacy.leaves != 0 {
		t.Error("left the legacy dialect without getting onto the sticky one")
	}
}

// Reconcile is for a bot already in the call; it must not drag one in.
func TestReconcileOutsideTheCallDoesNothing(t *testing.T) {
	want := []string{DialectSticky}
	s, legacy, sticky, conns := twoDialectSession(t, &want)
	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if legacy.joins+sticky.joins != 0 || len(conns) != 0 || s.Joined() {
		t.Error("Reconcile() joined a call the bot was not in")
	}
}
