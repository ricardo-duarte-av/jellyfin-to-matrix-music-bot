package rtc

import (
	"context"
	"crypto/rand"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"
)

const (
	// MediaKeyBytes is the size of the raw key material handed to other
	// members. LiveKit derives the AES-GCM-128 key from it with HKDF.
	MediaKeyBytes = 16
	// mediaKeyIndices is the key ring size: the index travels in one byte of
	// every frame's trailer, so it wraps at 256.
	mediaKeyIndices = 256
	// seedKeyIndex holds a key nobody is ever told, so the frame encryptor has
	// something to start from before the first real key exists. It is the
	// index the first real key does not use, so the encryptor, which caches its
	// cipher by index, never mistakes one for the other.
	seedKeyIndex = mediaKeyIndices - 1
)

// MediaKeys is the bot's own frame encryption keys, as the LiveKit frame
// encryptor reads them.
//
// Keys are stored derived, the way Element Call's MatrixKeyProvider sets them:
// the raw 16 bytes that go out to other members run through HKDF-SHA256 with
// LiveKit's salt, so the receiving side, which derives the same way, ends up
// with the same AES key.
type MediaKeys struct {
	mu      sync.RWMutex
	keys    map[uint32][]byte
	current atomic.Uint32
}

// NewMediaKeys starts with a throwaway key, so an encryptor can be built before
// the first key has been shared with anybody.
func NewMediaKeys() (*MediaKeys, error) {
	k := &MediaKeys{keys: make(map[uint32][]byte)}
	seed := make([]byte, MediaKeyBytes)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	if err := k.Use(seedKeyIndex, seed); err != nil {
		return nil, err
	}
	return k, nil
}

// GetKey returns the derived key at index.
func (k *MediaKeys) GetKey(index uint32) ([]byte, error) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	key, ok := k.keys[index]
	if !ok {
		return nil, fmt.Errorf("no media key at index %d", index)
	}
	return key, nil
}

// CurrentKeyIndex is the index outgoing frames are encrypted under.
func (k *MediaKeys) CurrentKeyIndex() uint32 { return k.current.Load() }

// Use derives raw into the key at index and encrypts with it from now on.
func (k *MediaKeys) Use(index int, raw []byte) error {
	derived, err := lksdk.DeriveKeyFromBytes(raw)
	if err != nil {
		return fmt.Errorf("derive media key: %w", err)
	}
	k.mu.Lock()
	k.keys[uint32(index)] = derived
	k.mu.Unlock()
	k.current.Store(uint32(index))
	return nil
}

// FrameEncryptor builds an encryptor for one track. Each track gets its own:
// an encryptor caches the cipher for the index it last used.
func (k *MediaKeys) FrameEncryptor(codec lksdk.Codec) (frameEncryptor, error) {
	return lksdk.NewFrameEncryptor(k, codec)
}

// frameEncryptor is lksdk's frame encryptor, named here so publisher.go does not
// need the e2ee types package for one interface.
type frameEncryptor = interface {
	EncryptFrame(payload []byte) ([]byte, error)
}

// KeyTarget is one device in the call that has to be able to decrypt the bot.
type KeyTarget struct {
	User   id.UserID
	Device id.DeviceID
	// Session changes when the device leaves and comes back, which looks
	// otherwise like it never went: a client that rejoined lost the keys it
	// had and needs them again.
	Session string
}

// KeySender delivers a media key to devices, as Olm-encrypted to-device
// messages. memberIDs names the memberships the bot is in the call on, so each
// receiving client can attach the key to whichever of them it reads.
//
// It reports the devices it could not reach. An error with none reported means
// none were reached.
type KeySender func(ctx context.Context, targets []KeyTarget, memberIDs []string, index int, key []byte) (unreached []KeyTarget, err error)

// Rotation timing, as matrix-js-sdk's RTCEncryptionManager has it.
const (
	// keyRotationGrace is how young the current key must be for a newcomer to
	// simply be handed it. A busier window than this and everybody gets a new
	// key instead, so the newcomer cannot decrypt what was played before they
	// arrived.
	keyRotationGrace = 10 * time.Second
	// keyUseDelay is how long a new key is held back after sending it, so
	// receivers have it before the first frame encrypted with it arrives.
	keyUseDelay = 5 * time.Second
	// keyRetryDelay is the first wait before trying again to reach devices a
	// send failed for. It doubles with every attempt, up to keyRetries.
	keyRetryDelay = 5 * time.Second
	keyRetries    = 5
)

// KeyShare decides when the bot's media key changes and who is told.
//
// It follows Element Call's rules, which xmuks also follows:
//
//   - somebody joins: they get the current key if it is young, otherwise
//     everyone gets a new one;
//   - somebody leaves: everyone gets a new one, so the leaver cannot decrypt
//     what comes next;
//   - a new key is used a few seconds after it is sent, except the first after
//     entering the call, which is used straight away: nothing has been shared
//     before it.
//
// Updates are cheap and never block on the network. A worker goroutine applies
// the latest one, so a burst of membership changes is a single decision and a
// slow homeserver holds up nothing but itself.
type KeyShare struct {
	log   zerolog.Logger
	keys  *MediaKeys
	send  KeySender
	clock func() time.Time

	grace, useDelay, retryDelay time.Duration

	mu sync.Mutex
	// latest is the most recent picture of the call, waiting to be applied.
	latest keySnapshot
	wake   chan struct{}

	// The rest belongs to the worker.
	members   map[keyDevice]string
	memberIDs []string
	index     int
	key       []byte
	createdAt time.Time
	// fresh means no key has been shared since the bot entered the call, so
	// the next one is used immediately.
	fresh bool
	// useTimer is the pending switch to a key that has just been sent.
	useTimer *time.Timer
	// unreached is the devices in the call the current key has not got to
	// yet, with how many times it has failed to.
	unreached map[keyDevice]*unreachedDevice
}

type unreachedDevice struct {
	target   KeyTarget
	attempts int
	next     time.Time
}

type keyDevice struct {
	user   id.UserID
	device id.DeviceID
}

type keySnapshot struct {
	inCall    bool
	targets   []KeyTarget
	memberIDs []string
}

// NewKeyShare shares the keys in keys through send.
func NewKeyShare(log zerolog.Logger, keys *MediaKeys, send KeySender) *KeyShare {
	return &KeyShare{
		log:        log,
		keys:       keys,
		send:       send,
		clock:      time.Now,
		grace:      keyRotationGrace,
		useDelay:   keyUseDelay,
		retryDelay: keyRetryDelay,
		wake:       make(chan struct{}, 1),
		members:    make(map[keyDevice]string),
		unreached:  make(map[keyDevice]*unreachedDevice),
		index:      -1,
		fresh:      true,
	}
}

// Update reports the call as it is now: the devices in it other than the bot's
// own, and the memberships the bot is on. memberIDs empty means the bot is not
// in the call.
func (s *KeyShare) Update(targets []KeyTarget, memberIDs []string) {
	s.mu.Lock()
	s.latest = keySnapshot{
		inCall:    len(memberIDs) > 0,
		targets:   slices.Clone(targets),
		memberIDs: slices.Sorted(slices.Values(memberIDs)),
	}
	s.mu.Unlock()
	s.kick()
}

func (s *KeyShare) kick() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run applies updates until ctx is done.
func (s *KeyShare) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
			s.mu.Lock()
			snap := s.latest
			s.mu.Unlock()
			s.apply(ctx, snap)
		}
	}
}

// apply brings what has been shared in line with snap.
func (s *KeyShare) apply(ctx context.Context, snap keySnapshot) {
	if !snap.inCall {
		// Out of the call. Whoever is there next gets a fresh key, used at
		// once, whatever the age of this one.
		clear(s.members)
		clear(s.unreached)
		s.memberIDs = nil
		s.fresh = true
		return
	}

	current := make(map[keyDevice]string, len(snap.targets))
	byDevice := make(map[keyDevice]KeyTarget, len(snap.targets))
	for _, t := range snap.targets {
		d := keyDevice{t.User, t.Device}
		current[d] = t.Session
		byDevice[d] = t
	}
	var joined []KeyTarget
	left := false
	for d, session := range current {
		if prev, ok := s.members[d]; !ok || prev != session {
			joined = append(joined, byDevice[d])
		}
	}
	for d := range s.members {
		if _, ok := current[d]; !ok {
			left = true
			delete(s.unreached, d)
		}
	}
	movedDialects := !slices.Equal(s.memberIDs, snap.memberIDs)
	s.members = current
	s.memberIDs = snap.memberIDs
	all := slices.Collect(maps.Values(byDevice))

	switch {
	case s.key == nil || s.fresh || left:
		s.rotate(ctx, all)
	case len(joined) > 0 && s.clock().Sub(s.createdAt) >= s.grace:
		s.rotate(ctx, all)
	case movedDialects:
		// The bot is on different memberships than when the key went out,
		// and a client files a key under the membership it saw at the time.
		// Everybody gets it again under the new ones.
		s.deliver(ctx, all)
	case len(joined) > 0:
		s.deliver(ctx, joined)
	}
	s.retryUnreached(ctx)
}

// retryUnreached tries again the devices whose retry is due.
func (s *KeyShare) retryUnreached(ctx context.Context) {
	now := s.clock()
	var due []KeyTarget
	for _, u := range s.unreached {
		if !now.Before(u.next) {
			due = append(due, u.target)
		}
	}
	if len(due) > 0 {
		s.deliver(ctx, due)
	}
}

// rotate makes a new key and sends it to targets.
func (s *KeyShare) rotate(ctx context.Context, targets []KeyTarget) {
	raw := make([]byte, MediaKeyBytes)
	if _, err := rand.Read(raw); err != nil {
		s.log.Err(err).Msg("could not generate a media key")
		return
	}
	s.index = (s.index + 1) % mediaKeyIndices
	if s.index == seedKeyIndex {
		// Never reuse the seed's slot: an encryptor that started on the seed
		// would go on using its cached cipher.
		s.index = 0
	}
	s.key = raw
	s.createdAt = s.clock()
	index := s.index

	s.deliver(ctx, targets)

	if s.useTimer != nil {
		s.useTimer.Stop()
		s.useTimer = nil
	}
	if s.fresh {
		s.fresh = false
		s.use(index, raw)
		return
	}
	s.useTimer = time.AfterFunc(s.useDelay, func() { s.use(index, raw) })
}

func (s *KeyShare) use(index int, raw []byte) {
	if err := s.keys.Use(index, raw); err != nil {
		s.log.Err(err).Msg("could not switch media key")
		return
	}
	s.log.Debug().Int("index", index).Msg("encrypting call media with a new key")
}

// deliver sends the current key to targets, and schedules another go at the
// devices it could not reach.
//
// A retry hands over the same key rather than rotating: a device that cannot be
// reached is not one that left, and treating it as a fresh arrival would rotate
// everybody's key on every attempt.
func (s *KeyShare) deliver(ctx context.Context, targets []KeyTarget) {
	if len(targets) == 0 || s.key == nil {
		return
	}
	unreached, err := s.send(ctx, targets, s.memberIDs, s.index, s.key)
	if err != nil && len(unreached) == 0 {
		unreached = targets
	}
	previous := make(map[keyDevice]int, len(targets))
	for _, t := range targets {
		d := keyDevice{t.User, t.Device}
		if u, ok := s.unreached[d]; ok {
			previous[d] = u.attempts
		}
		delete(s.unreached, d)
	}
	if len(unreached) == 0 {
		s.log.Debug().Int("index", s.index).Int("devices", len(targets)).Msg("shared the media key")
		return
	}

	var wait time.Duration
	for _, t := range unreached {
		d := keyDevice{t.User, t.Device}
		attempts := previous[d] + 1
		if attempts > keyRetries {
			s.log.Warn().Str("user_id", t.User.String()).Str("device_id", t.Device.String()).
				Msg("giving up on sharing the media key with a device; it will not hear the bot")
			continue
		}
		delay := s.retryDelay << (attempts - 1)
		s.unreached[d] = &unreachedDevice{target: t, attempts: attempts, next: s.clock().Add(delay)}
		if wait == 0 || delay < wait {
			wait = delay
		}
	}
	s.log.Warn().Err(err).Int("devices", len(unreached)).Msg("could not share the media key with every device")
	if wait > 0 {
		time.AfterFunc(wait, s.kick)
	}
}
