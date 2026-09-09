package rtc

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

const (
	// redialInitial and redialMax bound the wait between reconnection attempts.
	// The first wait is short because the common cause is an SFU restart, which
	// is usually over in seconds; the cap keeps a long outage down to one
	// attempt a minute.
	redialInitial = 2 * time.Second
	redialMax     = time.Minute
	// redialTimeout bounds one attempt: a fresh OpenID token, a token exchange
	// with the authorization service, and the LiveKit handshake.
	redialTimeout = 30 * time.Second
)

// MultiPublisher publishes the same media through several LiveKit connections.
//
// It exists because the two MatrixRTC dialects derive different LiveKit
// participant identities for the same bot — "<user>:<device>" for the legacy
// stack, a hash over (user, device, member.id) for MSC4195 — and one connection
// can only be one identity. Publishing both memberships from a single connection
// would leave half the room looking at a tile that never makes a sound, so the
// bot connects once per identity and sends each Opus frame down both.
//
// ffmpeg still runs once; it is only the upstream that is duplicated.
//
// It also owns keeping those connections up. Nothing else can: a LiveKit
// connection that dies takes no membership with it, so the bot goes on
// advertising itself in the call — refreshing its membership, holding its tile
// — while publishing into a socket that no longer exists.
type MultiPublisher struct {
	log zerolog.Logger

	mu   sync.Mutex
	legs []*leg
	// videoName and keyframe are what a reconnected leg has to be told to
	// catch up on: a new connection starts with no video track and a blank
	// tile, and neither the player nor the artwork publisher re-sends on its
	// own.
	videoName string
	keyframe  []byte
	closed    bool
	// backoff overrides the wait between reconnect attempts; nil means
	// redialWait. Only tests set it.
	backoff func(attempt int) time.Duration
	// done interrupts a backoff wait at shutdown.
	done chan struct{}
}

// publisherLeg is the part of *Publisher that MultiPublisher drives. Naming it
// keeps the fan-out testable without a LiveKit server.
type publisherLeg interface {
	WriteOpus(frame []byte) error
	ShowImage(keyframe []byte) error
	PublishVideo(name string) error
	Identity() string
	OnLost(fn func())
	Close()
}

// dialer opens a replacement connection for a leg. It re-runs the whole join:
// the LiveKit token is minted from a one-shot OpenID token, so an old one
// cannot be reused.
type dialer func(ctx context.Context) (publisherLeg, error)

// leg is one connection and its state. A leg that fails is retired from the
// fan-out — the others carry the room meanwhile — and redialled in the
// background until it comes back.
type leg struct {
	name string
	pub  publisherLeg
	dial dialer
	dead bool
	// redialing guards against two reconnect loops for one leg: the failure
	// usually shows up twice, once as a write error and once as a disconnect.
	redialing bool
}

// NamedPublisher is a connection, the name it goes by in logs, and how to open
// it again.
type NamedPublisher struct {
	Name      string
	Publisher *Publisher
	// Dial reconnects this leg after the connection drops. A leg without one is
	// never reconnected.
	Dial func(ctx context.Context) (*Publisher, error)
}

// NewMultiPublisher groups already-connected publishers, in the given order.
// Nil publishers are skipped, so callers can pass a leg that was never
// established without checking first.
func NewMultiPublisher(log zerolog.Logger, publishers ...NamedPublisher) *MultiPublisher {
	m := &MultiPublisher{log: log, done: make(chan struct{})}
	for _, p := range publishers {
		if p.Publisher == nil {
			continue
		}
		l := &leg{name: p.Name, pub: p.Publisher}
		if dial := p.Dial; dial != nil {
			l.dial = func(ctx context.Context) (publisherLeg, error) { return dial(ctx) }
		}
		m.legs = append(m.legs, l)
		m.watch(l, p.Publisher)
	}
	return m
}

// newMultiPublisher is the same over anything shaped like a publisher.
func newMultiPublisher(log zerolog.Logger, legs ...*leg) *MultiPublisher {
	m := &MultiPublisher{log: log, done: make(chan struct{}), legs: legs}
	for _, l := range legs {
		m.watch(l, l.pub)
	}
	return m
}

// watch asks a connection to report its own death. A dropped connection is
// otherwise invisible until something tries to write to it, and an idle bot
// writes nothing at all — so without this the bot could sit out of the call for
// as long as the queue is empty and only discover it on the next !play.
func (m *MultiPublisher) watch(l *leg, pub publisherLeg) {
	pub.OnLost(func() { m.lost(l, fmt.Errorf("livekit disconnected")) })
}

// lost retires a leg and starts reconnecting it.
func (m *MultiPublisher) lost(l *leg, cause error) {
	m.mu.Lock()
	if m.closed || l.dead {
		m.mu.Unlock()
		return
	}
	l.dead = true
	m.log.Warn().Err(cause).Str("leg", l.name).Msg("lost the livekit connection; reconnecting")
	m.startRedialLocked(l)
	m.mu.Unlock()
}

// startRedialLocked launches the reconnect loop for a retired leg, unless one
// is already running or the leg cannot be redialled. Caller must hold m.mu.
func (m *MultiPublisher) startRedialLocked(l *leg) {
	if m.closed || l.dial == nil || l.redialing {
		return
	}
	l.redialing = true
	go m.redial(l)
}

// redial reconnects a leg, backing off between attempts, until it succeeds or
// the publisher is closed. It never gives up: the alternative is a bot that
// looks like it is in the call for as long as it runs but cannot be heard.
func (m *MultiPublisher) redial(l *leg) {
	defer func() {
		m.mu.Lock()
		l.redialing = false
		m.mu.Unlock()
	}()

	for attempt := 1; ; attempt++ {
		select {
		case <-m.done:
			return
		case <-time.After(m.waitFor(attempt)):
		}

		ctx, cancel := context.WithTimeout(context.Background(), redialTimeout)
		pub, err := l.dial(ctx)
		cancel()
		if err != nil {
			m.log.Warn().Err(err).Str("leg", l.name).Int("attempt", attempt).
				Msg("could not reconnect to livekit; will retry")
			continue
		}
		if !m.adopt(l, pub) {
			// Closed while we were dialling.
			pub.Close()
			return
		}
		m.log.Info().Str("leg", l.name).Str("identity", pub.Identity()).Int("attempt", attempt).
			Msg("reconnected to livekit")
		return
	}
}

// waitFor is how long to hold off before the given attempt, counting from one.
func (m *MultiPublisher) waitFor(attempt int) time.Duration {
	m.mu.Lock()
	backoff := m.backoff
	m.mu.Unlock()
	if backoff != nil {
		return backoff(attempt)
	}
	return redialWait(attempt)
}

// redialWait doubles the wait each attempt, up to the cap.
func redialWait(attempt int) time.Duration {
	wait := redialInitial
	for i := 1; i < attempt && wait < redialMax; i++ {
		wait *= 2
	}
	return min(wait, redialMax)
}

// adopt puts a fresh connection back into the fan-out and restores the video
// track it missed. It reports false if the publisher closed meanwhile.
func (m *MultiPublisher) adopt(l *leg, pub publisherLeg) bool {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return false
	}
	old := l.pub
	l.pub, l.dead = pub, false
	name, keyframe := m.videoName, m.keyframe
	m.mu.Unlock()

	if old != nil {
		old.Close()
	}
	m.watch(l, pub)

	if name != "" {
		if err := pub.PublishVideo(name); err != nil {
			m.log.Warn().Err(err).Str("leg", l.name).Msg("could not republish the album art video track")
		} else if len(keyframe) > 0 {
			if err := pub.ShowImage(keyframe); err != nil {
				m.log.Warn().Err(err).Str("leg", l.name).Msg("could not restore the album art still")
			}
		}
	}
	return true
}

// WriteOpus sends one encoded Opus frame down every live connection.
//
// A failing leg must never stop playback for the rest of the room, so an error
// retires that leg and the frame still goes to the survivors. Only the loss of
// every leg is reported to the player.
func (m *MultiPublisher) WriteOpus(frame []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	alive := 0
	for _, l := range m.legs {
		if l.dead {
			continue
		}
		if err := l.pub.WriteOpus(frame); err != nil {
			l.dead = true
			m.log.Warn().Err(err).Str("leg", l.name).Msg("livekit connection stopped accepting audio; reconnecting")
			m.startRedialLocked(l)
			continue
		}
		alive++
	}
	if alive == 0 {
		return fmt.Errorf("no live livekit connection")
	}
	return nil
}

// ShowImage displays a keyframe on every connection that has a video track.
func (m *MultiPublisher) ShowImage(keyframe []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.keyframe = keyframe
	var firstErr error
	for _, l := range m.legs {
		if l.dead {
			continue
		}
		if err := l.pub.ShowImage(keyframe); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", l.name, err)
		}
	}
	return firstErr
}

// PublishVideo adds the still-image track to every connection. The artwork
// track is decorative, so a leg that refuses it stays in the call as audio.
func (m *MultiPublisher) PublishVideo(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.videoName = name
	published := 0
	var firstErr error
	for _, l := range m.legs {
		if l.dead {
			continue
		}
		if err := l.pub.PublishVideo(name); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", l.name, err)
			}
			continue
		}
		published++
	}
	if published == 0 && firstErr != nil {
		return firstErr
	}
	if firstErr != nil {
		m.log.Warn().Err(firstErr).Msg("album art video track unavailable on one livekit connection")
	}
	return nil
}

// Identity lists the LiveKit identities in use, one per connection.
func (m *MultiPublisher) Identity() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	parts := make([]string, 0, len(m.legs))
	for _, l := range m.legs {
		parts = append(parts, fmt.Sprintf("%s=%s", l.name, l.pub.Identity()))
	}
	return strings.Join(parts, " ")
}

// Close disconnects every connection.
func (m *MultiPublisher) Close() {
	m.mu.Lock()
	legs := m.legs
	m.legs = nil
	if !m.closed {
		m.closed = true
		close(m.done)
	}
	m.mu.Unlock()

	for _, l := range legs {
		l.pub.Close()
	}
}
