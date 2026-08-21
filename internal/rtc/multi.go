package rtc

import (
	"fmt"
	"strings"
	"sync"

	"github.com/rs/zerolog"
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
type MultiPublisher struct {
	log zerolog.Logger

	mu   sync.Mutex
	legs []*leg
}

// publisherLeg is the part of *Publisher that MultiPublisher drives. Naming it
// keeps the fan-out testable without a LiveKit server.
type publisherLeg interface {
	WriteOpus(frame []byte) error
	ShowImage(keyframe []byte) error
	PublishVideo(name string) error
	Identity() string
	Close()
}

// leg is one connection, plus whether it has already failed. A leg that starts
// erroring is dropped rather than retried: the others carry the room.
type leg struct {
	name string
	pub  publisherLeg
	dead bool
}

// NamedPublisher is a connection and the name it goes by in logs.
type NamedPublisher struct {
	Name      string
	Publisher *Publisher
}

// NewMultiPublisher groups already-connected publishers, in the given order.
// Nil publishers are skipped, so callers can pass a leg that was never
// established without checking first.
func NewMultiPublisher(log zerolog.Logger, publishers ...NamedPublisher) *MultiPublisher {
	m := &MultiPublisher{log: log}
	for _, p := range publishers {
		if p.Publisher != nil {
			m.legs = append(m.legs, &leg{name: p.Name, pub: p.Publisher})
		}
	}
	return m
}

// newMultiPublisher is the same over anything shaped like a publisher.
func newMultiPublisher(log zerolog.Logger, legs ...*leg) *MultiPublisher {
	return &MultiPublisher{log: log, legs: legs}
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
			m.log.Warn().Err(err).Str("leg", l.name).Msg("livekit connection stopped accepting audio; dropping it")
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
	m.mu.Unlock()

	for _, l := range legs {
		l.pub.Close()
	}
}
