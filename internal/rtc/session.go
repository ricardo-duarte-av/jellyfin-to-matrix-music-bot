package rtc

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/rs/zerolog"
)

// Focus is the LiveKit service a call is running on, and the room on it.
//
// It is a holder rather than a value because it changes: the bot leaves an
// empty call and rejoins when someone comes back, and by then whoever started
// the call has chosen a focus that the bot has to follow. Everything that mints
// a token reads it at the moment it dials rather than closing over a URL from
// startup.
type Focus struct {
	mu sync.Mutex
	// preferred is the configured or discovered service, used when there is no
	// call in progress to defer to.
	preferred  string
	serviceURL string
	// alias is the LiveKit room the token service put the bot in. It is
	// published in the membership so other clients land in the same room, and
	// it is only known once a token has been minted.
	alias string
}

// NewFocus holds preferred as the service to use when the bot picks for itself.
func NewFocus(preferred string) *Focus {
	return &Focus{preferred: preferred, serviceURL: preferred}
}

// ServiceURL is the MatrixRTC authorization service to mint tokens against.
func (f *Focus) ServiceURL() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.serviceURL != "" {
		return f.serviceURL
	}
	return f.preferred
}

// Preferred is the service the bot proposes when nobody else is in the call.
func (f *Focus) Preferred() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.preferred
}

// Use switches to the service an existing call is on. An empty URL keeps the
// preferred one.
func (f *Focus) Use(serviceURL string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if serviceURL == "" {
		serviceURL = f.preferred
	}
	f.serviceURL = serviceURL
}

// SetAlias records the LiveKit room a token was minted for.
func (f *Focus) SetAlias(alias string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alias = alias
}

// Transport is what a membership publishes so other clients can find the bot.
func (f *Focus) Transport() Transport {
	f.mu.Lock()
	defer f.mu.Unlock()
	return Transport{
		Type:              TransportTypeLiveKit,
		LiveKitServiceURL: f.serviceURL,
		LiveKitAlias:      f.alias,
	}
}

// The MatrixRTC dialects. Each names both a membership and the LiveKit leg
// behind it, which is how the session pairs the two up.
const (
	DialectLegacy = "legacy"
	DialectSticky = "sticky"
)

// CallMember is one dialect's membership: what the room is told about the bot
// being in the call. Both dialects publish, refresh and retract one.
type CallMember interface {
	// Dialect names the membership, and the publisher leg that carries it.
	Dialect() string
	// Join publishes the membership. transport is the focus the bot proposes;
	// a dialect that keeps its focus elsewhere ignores it.
	Join(ctx context.Context, transport Transport) error
	Leave(ctx context.Context) error
}

// StickyMember adapts a sticky membership to CallMember. Its focus lives in the
// slot, not in the membership, so it has no use for the transport.
func StickyMember(m *StickyMembership) CallMember { return stickyMember{m} }

type stickyMember struct{ *StickyMembership }

func (stickyMember) Dialect() string { return DialectSticky }

func (s stickyMember) Join(ctx context.Context, _ Transport) error {
	return s.StickyMembership.Join(ctx)
}

// Session is the bot's presence in a call: the memberships that announce it and
// the SFU connections behind them, entered and left as an audience comes and
// goes.
//
// Nothing here is created or destroyed by entering and leaving. The memberships
// and the fan-out outlive any one visit to the call, so the player can hold a
// publisher that is sometimes connected and sometimes not, rather than being
// rebuilt around a new one every time somebody joins.
type Session struct {
	log       zerolog.Logger
	focus     *Focus
	publisher *MultiPublisher
	members   []CallMember
	// resolve reports the service an existing call is already using, so the bot
	// joins on the focus the room agreed on rather than imposing its own. It is
	// asked on every entry: between two visits the call may have been started
	// by somebody else, on a different one.
	resolve func(ctx context.Context) (string, error)
	// choose names the dialects the rest of the call can see the bot in; nil,
	// or an answer naming none the bot has, means every dialect.
	choose func() []string

	mu     sync.Mutex
	joined bool
	// active is the dialects the bot is in the call on right now.
	active map[string]bool
}

// NewSession groups the memberships and connections that make up the bot's
// presence in a call.
func NewSession(log zerolog.Logger, focus *Focus, publisher *MultiPublisher, resolve func(ctx context.Context) (string, error), members ...CallMember) *Session {
	return &Session{log: log, focus: focus, publisher: publisher, resolve: resolve, members: members}
}

// ChooseDialects sets how the session picks the dialects to be in the call on.
// Without it the bot joins on all of them, which in a call whose clients read
// both shows it twice.
func (s *Session) ChooseDialects(choose func() []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.choose = choose
}

// wantedLocked is the set of dialects to be in the call on now. Caller must
// hold s.mu.
func (s *Session) wantedLocked() map[string]bool {
	all := make(map[string]bool, len(s.members))
	for _, member := range s.members {
		all[member.Dialect()] = true
	}
	if s.choose == nil {
		return all
	}
	wanted := make(map[string]bool)
	for _, name := range s.choose() {
		if all[name] {
			wanted[name] = true
		}
	}
	if len(wanted) == 0 {
		// Nobody to go by, or they only read a dialect this bot cannot speak:
		// being visible twice beats not being visible.
		return all
	}
	return wanted
}

// Joined reports whether the bot is currently in the call.
func (s *Session) Joined() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.joined
}

// Enter connects to the SFU and publishes the bot's memberships.
//
// The connection is made before the memberships are published, which is the
// opposite of what it looks like it should be. A membership is a promise that
// the bot can be heard; publishing one and then failing to connect advertises a
// participant that produces no sound, which is exactly the state all the
// reconnect machinery exists to avoid. The other way round the worst case is a
// moment of publishing into a call nobody has been told about yet.
func (s *Session) Enter(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.joined {
		return nil
	}

	if s.resolve != nil {
		serviceURL, err := s.resolve(ctx)
		if err != nil {
			s.log.Warn().Err(err).Msg("could not tell which focus the call is on; using the configured one")
		}
		s.focus.Use(serviceURL)
	}

	wanted := s.wantedLocked()
	s.publisher.SelectLegs(wanted)
	if err := s.publisher.Resume(ctx); err != nil {
		return fmt.Errorf("connect to livekit: %w", err)
	}

	transport := s.focus.Transport()
	var failed []string
	active := make(map[string]bool)
	for _, member := range s.members {
		if !wanted[member.Dialect()] {
			continue
		}
		if err := member.Join(ctx, transport); err != nil {
			failed = append(failed, err.Error())
			continue
		}
		active[member.Dialect()] = true
	}
	if len(active) == 0 {
		// Nobody can see the bot, so being connected is just cost. Undo it.
		s.publisher.Suspend()
		return fmt.Errorf("publish call membership: %s", strings.Join(failed, "; "))
	}
	s.joined = true
	s.active = active
	if len(failed) > 0 {
		s.log.Warn().Strs("errors", failed).Msg("joined the call on some memberships but not all")
	}
	s.log.Info().Str("focus", s.focus.ServiceURL()).Str("identities", s.publisher.Identity()).
		Msg("joined the call")
	return nil
}

// Reconcile moves a bot that is in the call onto the dialects the call now
// wants, and does nothing otherwise.
//
// The new dialects are joined before the old ones are left, so the bot is never
// missing from the call in between: a client briefly seeing it twice is better
// than one seeing it drop out. A dialect that cannot be joined keeps the old
// ones in place, since leaving them would leave that part of the room with
// nothing.
func (s *Session) Reconcile(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.joined {
		return nil
	}
	wanted := s.wantedLocked()
	if maps.Equal(wanted, s.active) {
		return nil
	}

	transport := s.focus.Transport()
	var failed []string
	for _, member := range s.members {
		name := member.Dialect()
		if !wanted[name] || s.active[name] {
			continue
		}
		if err := s.publisher.EnableLeg(ctx, name); err != nil {
			failed = append(failed, fmt.Sprintf("%s: connect: %v", name, err))
			continue
		}
		if err := member.Join(ctx, transport); err != nil {
			s.publisher.DisableLeg(name)
			failed = append(failed, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		s.active[name] = true
	}
	if len(failed) > 0 {
		return fmt.Errorf("switch call dialects: %s", strings.Join(failed, "; "))
	}

	for _, member := range s.members {
		name := member.Dialect()
		if wanted[name] || !s.active[name] {
			continue
		}
		if err := member.Leave(ctx); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", name, err))
		}
		// Dropped either way: the membership left behind is on its delayed
		// leave, and a connection behind a retracted membership is only cost.
		s.publisher.DisableLeg(name)
		delete(s.active, name)
	}
	s.log.Info().Strs("dialects", slices.Sorted(maps.Keys(s.active))).
		Str("identities", s.publisher.Identity()).Msg("switched call dialects")
	if len(failed) > 0 {
		return fmt.Errorf("leave call dialects: %s", strings.Join(failed, "; "))
	}
	return nil
}

// Leave retracts the memberships and drops the connections.
//
// The memberships go first: they are what the room reads, so retracting them
// is what actually takes the bot out of the call, and a connection that outlives
// them by a moment is invisible. The other order would leave a participant
// nobody can hear, however briefly.
func (s *Session) Leave(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.joined {
		return nil
	}
	s.joined = false

	var failed []string
	for _, member := range s.members {
		if !s.active[member.Dialect()] {
			continue
		}
		if err := member.Leave(ctx); err != nil {
			failed = append(failed, err.Error())
		}
	}
	s.active = nil
	s.publisher.Suspend()
	s.log.Info().Msg("left the call")
	if len(failed) > 0 {
		return fmt.Errorf("retract call membership: %s", strings.Join(failed, "; "))
	}
	return nil
}
