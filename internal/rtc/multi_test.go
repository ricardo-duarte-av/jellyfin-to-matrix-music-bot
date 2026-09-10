package rtc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// fakeLeg is one LiveKit connection that can be told to start failing.
//
// The reconnect tests drive it from two goroutines — the test itself and the
// redial loop, which closes the connection it replaces — so its counters are
// guarded and read back through the accessors below.
type fakeLeg struct {
	mu       sync.Mutex
	identity string
	frames   int
	images   int
	videos   int
	closed   bool
	err      error
	lost     func()
	// ready is closed the first time an image is shown, so a test can wait for
	// a reconnected leg to have caught up.
	ready     chan struct{}
	readyOnce sync.Once
}

func (f *fakeLeg) WriteOpus(frame []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.frames++
	return nil
}

func (f *fakeLeg) ShowImage(keyframe []byte) error {
	f.mu.Lock()
	if f.err != nil {
		f.mu.Unlock()
		return f.err
	}
	f.images++
	f.mu.Unlock()

	if f.ready != nil {
		f.readyOnce.Do(func() { close(f.ready) })
	}
	return nil
}

func (f *fakeLeg) PublishVideo(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.videos++
	return nil
}

func (f *fakeLeg) Identity() string { return f.identity }

func (f *fakeLeg) OnLost(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lost = fn
}

func (f *fakeLeg) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
}

// drop is the connection dying the way the SDK reports it.
func (f *fakeLeg) drop() {
	f.mu.Lock()
	f.err = errors.New("livekit disconnected")
	lost := f.lost
	f.mu.Unlock()

	if lost != nil {
		lost()
	}
}

// counts reports the frames, images and video tracks this leg has taken, and
// whether it has been closed.
func (f *fakeLeg) counts() (frames, images, videos int, closed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.frames, f.images, f.videos, f.closed
}

func (f *fakeLeg) frameCount() int {
	frames, _, _, _ := f.counts()
	return frames
}

func testMulti(legs ...*fakeLeg) (*MultiPublisher, []*leg) {
	wrapped := make([]*leg, len(legs))
	for i, l := range legs {
		wrapped[i] = &leg{name: fmt.Sprintf("leg%d", i), pub: l}
	}
	return newMultiPublisher(zerolog.New(io.Discard), wrapped...), wrapped
}

// testMultiDial is testMulti with a dialer, and a backoff short enough that a
// test does not wait seconds for the reconnect it is checking.
func testMultiDial(dial dialer, legs ...*fakeLeg) (*MultiPublisher, []*leg) {
	m, wrapped := testMulti(legs...)
	m.backoff = func(int) time.Duration { return time.Millisecond }
	for _, l := range wrapped {
		l.dial = dial
	}
	return m, wrapped
}

// waitFor polls until cond holds, or fails the test.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The whole point of the second connection: both identities publish the same
// audio, so neither dialect's clients are left with a silent tile.
func TestWriteOpusReachesEveryLeg(t *testing.T) {
	a, b := &fakeLeg{}, &fakeLeg{}
	m, _ := testMulti(a, b)

	if err := m.WriteOpus([]byte{1, 2, 3}); err != nil {
		t.Fatalf("WriteOpus() = %v", err)
	}
	if a.frames != 1 || b.frames != 1 {
		t.Errorf("frames written: %d and %d; want 1 each", a.frames, b.frames)
	}
}

// One SFU connection going bad must not take the music away from the room.
func TestWriteOpusSurvivesAFailingLeg(t *testing.T) {
	good, bad := &fakeLeg{}, &fakeLeg{err: fmt.Errorf("connection reset")}
	m, wrapped := testMulti(good, bad)

	if err := m.WriteOpus([]byte{1}); err != nil {
		t.Fatalf("WriteOpus() = %v; want the surviving leg to carry it", err)
	}
	if good.frames != 1 {
		t.Errorf("good leg got %d frames; want 1", good.frames)
	}
	if !wrapped[1].dead {
		t.Error("failing leg was not retired")
	}

	// A retired leg is not tried again.
	bad.err = nil
	if err := m.WriteOpus([]byte{2}); err != nil {
		t.Fatalf("WriteOpus() = %v", err)
	}
	if bad.frames != 0 {
		t.Errorf("retired leg received %d frames; want none", bad.frames)
	}
}

// Losing every connection is a real failure and the player should hear about it.
func TestWriteOpusFailsWhenEveryLegIsGone(t *testing.T) {
	a := &fakeLeg{err: fmt.Errorf("gone")}
	m, _ := testMulti(a)

	if err := m.WriteOpus([]byte{1}); err == nil {
		t.Fatal("WriteOpus() = nil with no live connection")
	}
	if err := m.WriteOpus([]byte{2}); err == nil {
		t.Fatal("WriteOpus() = nil on the second attempt with no live connection")
	}
}

// Artwork is decorative: a connection that will not take a video track stays in
// the call as audio rather than failing the whole publish.
func TestPublishVideoToleratesOneLeg(t *testing.T) {
	good, bad := &fakeLeg{}, &fakeLeg{err: fmt.Errorf("no video")}
	m, _ := testMulti(good, bad)

	if err := m.PublishVideo("Jukebox"); err != nil {
		t.Fatalf("PublishVideo() = %v; want the working leg to be enough", err)
	}
	if good.videos != 1 {
		t.Errorf("good leg published %d video tracks; want 1", good.videos)
	}

	// No leg accepting it is a different matter.
	onlyBad, _ := testMulti(&fakeLeg{err: fmt.Errorf("no video")})
	if err := onlyBad.PublishVideo("Jukebox"); err == nil {
		t.Error("PublishVideo() = nil when no leg accepted the track")
	}
}

func TestIdentityNamesEveryLeg(t *testing.T) {
	m, _ := testMulti(&fakeLeg{identity: "@bot:example.org:DEVICE"}, &fakeLeg{identity: "hashed"})

	got := m.Identity()
	for _, want := range []string{"@bot:example.org:DEVICE", "hashed"} {
		if !strings.Contains(got, want) {
			t.Errorf("Identity() = %q; missing %q", got, want)
		}
	}
}

func TestCloseClosesEveryLeg(t *testing.T) {
	a, b := &fakeLeg{}, &fakeLeg{}
	m, _ := testMulti(a, b)

	m.Close()
	if !a.closed || !b.closed {
		t.Errorf("closed: %v and %v; want both", a.closed, b.closed)
	}
}

// A connection that dies must be replaced, not merely retired: the bot's
// membership stays published either way, so a leg that never comes back is a
// participant that is in the call and cannot be heard for as long as the bot
// runs.
func TestLostLegIsReconnected(t *testing.T) {
	first, second := &fakeLeg{identity: "first"}, &fakeLeg{identity: "second"}
	m, _ := testMultiDial(func(context.Context) (publisherLeg, error) { return second, nil }, first)
	defer m.Close()

	first.drop()

	waitFor(t, "the replacement leg to receive audio", func() bool {
		_ = m.WriteOpus([]byte{1})
		return second.frameCount() > 0
	})
	if _, _, _, closed := first.counts(); !closed {
		t.Error("the dead connection was left open")
	}
}

// A reconnected leg starts with no video track and a blank tile. Nothing
// re-sends the cover on its own, so the fan-out has to replay it.
func TestReconnectRestoresTheVideoTile(t *testing.T) {
	first, second := &fakeLeg{}, &fakeLeg{ready: make(chan struct{})}
	m, _ := testMultiDial(func(context.Context) (publisherLeg, error) { return second, nil }, first)
	defer m.Close()

	if err := m.PublishVideo("Jukebox"); err != nil {
		t.Fatalf("PublishVideo() = %v", err)
	}
	if err := m.ShowImage([]byte{0x42}); err != nil {
		t.Fatalf("ShowImage() = %v", err)
	}

	first.drop()

	select {
	case <-second.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the tile to be restored on the new connection")
	}
	_, images, videos, _ := second.counts()
	if videos != 1 {
		t.Errorf("replacement leg published %d video tracks; want 1", videos)
	}
	if images != 1 {
		t.Errorf("replacement leg showed %d images; want 1", images)
	}
}

// A leg keeps trying: an SFU that is still restarting refuses the first dial.
func TestReconnectRetriesUntilTheSFUIsBack(t *testing.T) {
	var attempts atomic.Int32
	first, second := &fakeLeg{}, &fakeLeg{}
	dial := func(context.Context) (publisherLeg, error) {
		if attempts.Add(1) < 3 {
			return nil, errors.New("connection refused")
		}
		return second, nil
	}
	m, _ := testMultiDial(dial, first)
	defer m.Close()

	first.drop()

	waitFor(t, "the replacement leg to receive audio", func() bool {
		_ = m.WriteOpus([]byte{1})
		return second.frameCount() > 0
	})
	if got := attempts.Load(); got < 3 {
		t.Errorf("dialled %d times; want the failures to have been retried", got)
	}
}

// Shutting down must not leave a reconnect loop running against a bot that is
// already out of the call.
func TestCloseStopsReconnecting(t *testing.T) {
	var attempts atomic.Int32
	first := &fakeLeg{}
	dial := func(context.Context) (publisherLeg, error) {
		attempts.Add(1)
		return nil, errors.New("still down")
	}
	m, _ := testMultiDial(dial, first)

	first.drop()
	m.Close()

	time.Sleep(200 * time.Millisecond)
	settled := attempts.Load()
	time.Sleep(200 * time.Millisecond)
	if got := attempts.Load(); got != settled {
		t.Errorf("dial attempts went from %d to %d after Close", settled, got)
	}
}

// Leaving the call has to actually drop the connections — the whole point is
// not to hold an SFU connection open for nobody.
func TestSuspendClosesEveryLeg(t *testing.T) {
	a, b := &fakeLeg{}, &fakeLeg{}
	m, _ := testMulti(a, b)

	m.Suspend()

	if _, _, _, closed := a.counts(); !closed {
		t.Error("first leg was left connected")
	}
	if _, _, _, closed := b.counts(); !closed {
		t.Error("second leg was left connected")
	}
}

// A frame still in flight from the encoder when the bot leaves is not the call
// falling apart, so it must not be reported as such: the player is paused by
// the same rule that suspended this, and an error here would look like every
// connection having died.
func TestWritesAreDroppedWhileSuspended(t *testing.T) {
	a := &fakeLeg{}
	m, _ := testMulti(a)

	m.Suspend()

	if err := m.WriteOpus([]byte{1}); err != nil {
		t.Errorf("WriteOpus() while suspended = %v; want the frame dropped quietly", err)
	}
	if frames := a.frameCount(); frames != 0 {
		t.Errorf("a suspended leg took %d frames; want none", frames)
	}
}

// Rejoining has to put back everything the tile needs, or the bot comes back as
// a blank square: a new connection has no video track and nothing on it.
func TestResumeReconnectsAndRestoresTheTile(t *testing.T) {
	fresh := &fakeLeg{identity: "back"}
	m, _ := testMultiDial(func(context.Context) (publisherLeg, error) { return fresh, nil }, &fakeLeg{})
	if err := m.PublishVideo("Jukebox"); err != nil {
		t.Fatal(err)
	}
	if err := m.ShowImage([]byte{9}); err != nil {
		t.Fatal(err)
	}

	m.Suspend()
	if err := m.Resume(context.Background()); err != nil {
		t.Fatalf("Resume() = %v", err)
	}

	if err := m.WriteOpus([]byte{1}); err != nil {
		t.Fatalf("WriteOpus() after resuming = %v", err)
	}
	frames, images, videos, _ := fresh.counts()
	if frames != 1 {
		t.Errorf("the reconnected leg took %d frames; want 1", frames)
	}
	if videos != 1 {
		t.Errorf("republished the video track %d times; want 1", videos)
	}
	if images != 1 {
		t.Errorf("restored the album art %d times; want 1", images)
	}
}

// A resumption that cannot get in anywhere leaves the bot out of the call
// rather than half in it: the caller gives up on entering, and nothing must be
// left redialling behind its back.
func TestResumeThatConnectsNowhereStaysSuspended(t *testing.T) {
	m, _ := testMultiDial(func(context.Context) (publisherLeg, error) {
		return nil, errors.New("no token for you")
	}, &fakeLeg{})

	m.Suspend()
	if err := m.Resume(context.Background()); err == nil {
		t.Fatal("Resume() = nil with every leg refusing to connect")
	}

	m.mu.Lock()
	suspended := m.suspended
	m.mu.Unlock()
	if !suspended {
		t.Error("a resumption that connected nowhere left the publisher live")
	}
}

// The bot can leave the call while a reconnect is still in flight — the SFU
// dropped, the redial loop is mid-dial, and the last listener walks out. The
// connection that arrives afterwards belongs to a call the bot is no longer in.
func TestARedialInFlightDoesNotSurviveSuspension(t *testing.T) {
	dialed := make(chan struct{})
	release := make(chan struct{})
	late := &fakeLeg{identity: "late"}
	m, wrapped := testMultiDial(func(context.Context) (publisherLeg, error) {
		close(dialed)
		<-release
		return late, nil
	}, &fakeLeg{})

	// Drop the connection so the redial loop starts, and wait until it is
	// actually inside the dial.
	wrapped[0].pub.(*fakeLeg).drop()
	select {
	case <-dialed:
	case <-time.After(5 * time.Second):
		t.Fatal("the redial never started")
	}

	m.Suspend()
	close(release)

	waitFor(t, "the late connection to be discarded", func() bool {
		_, _, _, closed := late.counts()
		return closed
	})
	m.mu.Lock()
	adopted := wrapped[0].pub
	m.mu.Unlock()
	if adopted != nil {
		t.Error("a connection dialled before the bot left was adopted after it")
	}
}
