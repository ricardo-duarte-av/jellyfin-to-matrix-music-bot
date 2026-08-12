package player

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/jellyfin"
)

// fakePublisher records the frames the player writes.
type fakePublisher struct {
	mu     sync.Mutex
	frames int
	bytes  int
	err    error
}

func (f *fakePublisher) WriteOpus(frame []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.frames++
	f.bytes += len(frame)
	return nil
}

func (f *fakePublisher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.frames
}

// makeTone writes a WAV file of the given duration to use as a fake track.
func makeTone(t *testing.T, dir, name string, seconds int, freq int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=%d:duration=%d:sample_rate=48000", freq, seconds),
		"-ac", "2", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate tone: %v: %s", err, out)
	}
	return path
}

// testEncode mirrors the shipped defaults.
var testEncode = EncodeOptions{Bitrate: "128k", VBR: "constrained", Channels: 2}

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
}

func newTestPlayer(t *testing.T, pub Publisher) *Player {
	t.Helper()
	p := New(Options{
		Publisher:  pub,
		FFmpegPath: "ffmpeg",
		MaxQueue:   100,
		Encode:     testEncode,
		URLFor:     func(item jellyfin.Item) string { return item.ID },
	})
	t.Cleanup(p.Close)
	return p
}

// TestPlaybackEndToEnd runs the real pipeline: ffmpeg transcodes a tone to
// Ogg/Opus, the demuxer turns pages into frames, and the player paces them into
// the publisher. It is the closest thing to "is the bot audible" that does not
// need a LiveKit server.
func TestPlaybackEndToEnd(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	track := jellyfin.Item{ID: makeTone(t, dir, "tone.wav", 1, 440), Kind: jellyfin.KindTrack, Name: "Tone"}

	pub := &fakePublisher{}
	p := newTestPlayer(t, pub)

	added, truncated := p.Enqueue([]jellyfin.Item{track})
	if added != 1 || truncated {
		t.Fatalf("Enqueue() = %d, %v; want 1, false", added, truncated)
	}

	// One second of audio at 20ms per frame is ~50 frames. Allow slack for
	// scheduling, but require enough that we know real audio flowed.
	deadline := time.Now().Add(10 * time.Second)
	for p.Status().State != StateIdle && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	if got := pub.count(); got < 40 {
		t.Errorf("published %d frames; want at least 40 for a 1s track", got)
	}
	if pub.bytes == 0 {
		t.Error("published no audio bytes")
	}
	if state := p.Status().State; state != StateIdle {
		t.Errorf("state after queue drained = %q; want %q", state, StateIdle)
	}
}

func TestPauseStopsPublishing(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	track := jellyfin.Item{ID: makeTone(t, dir, "tone.wav", 5, 440), Kind: jellyfin.KindTrack, Name: "Tone"}

	pub := &fakePublisher{}
	p := newTestPlayer(t, pub)
	p.Enqueue([]jellyfin.Item{track})

	// Wait for audio to actually start flowing.
	deadline := time.Now().Add(5 * time.Second)
	for pub.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if pub.count() == 0 {
		t.Fatal("playback never started")
	}

	if !p.Pause() {
		t.Fatal("Pause() = false; want true while playing")
	}
	if state := p.Status().State; state != StatePaused {
		t.Fatalf("state = %q; want %q", state, StatePaused)
	}

	// Let the pause settle, then confirm nothing more is published.
	time.Sleep(100 * time.Millisecond)
	frozen := pub.count()
	time.Sleep(300 * time.Millisecond)
	if got := pub.count(); got != frozen {
		t.Errorf("published %d more frames while paused", got-frozen)
	}

	if !p.Resume() {
		t.Fatal("Resume() = false; want true while paused")
	}
	time.Sleep(300 * time.Millisecond)
	if got := pub.count(); got <= frozen {
		t.Errorf("no frames published after resume (still %d)", got)
	}
}

func TestNextAdvancesAndStopsAtEnd(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	first := jellyfin.Item{ID: makeTone(t, dir, "a.wav", 5, 440), Kind: jellyfin.KindTrack, Name: "A"}
	second := jellyfin.Item{ID: makeTone(t, dir, "b.wav", 5, 880), Kind: jellyfin.KindTrack, Name: "B"}

	p := newTestPlayer(t, &fakePublisher{})
	p.Enqueue([]jellyfin.Item{first, second})

	if current := p.Status().Current; current == nil || current.Name != "A" {
		t.Fatalf("first track = %v; want A", current)
	}
	if !p.Next() {
		t.Fatal("Next() = false; want true with a track remaining")
	}
	if current := p.Status().Current; current == nil || current.Name != "B" {
		t.Fatalf("after Next(), current = %v; want B", current)
	}
	if p.Next() {
		t.Error("Next() = true at end of queue; want false")
	}
	if state := p.Status().State; state != StateIdle {
		t.Errorf("state at end of queue = %q; want %q", state, StateIdle)
	}
}

func TestSkipToAndClear(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	tone := makeTone(t, dir, "a.wav", 5, 440)
	tracks := []jellyfin.Item{
		{ID: tone, Kind: jellyfin.KindTrack, Name: "A"},
		{ID: tone, Kind: jellyfin.KindTrack, Name: "B"},
		{ID: tone, Kind: jellyfin.KindTrack, Name: "C"},
	}

	p := newTestPlayer(t, &fakePublisher{})
	p.Enqueue(tracks)

	item, err := p.SkipTo(3)
	if err != nil {
		t.Fatalf("SkipTo(3) error: %v", err)
	}
	if item.Name != "C" {
		t.Errorf("SkipTo(3) = %q; want C", item.Name)
	}
	if _, err := p.SkipTo(99); err == nil {
		t.Error("SkipTo(99) succeeded; want an out-of-range error")
	}

	// Clear keeps the current track (position 3) and drops what follows it.
	if removed := p.Clear(); removed != 0 {
		t.Errorf("Clear() removed %d; want 0 when the current track is last", removed)
	}
	if got := len(p.Status().Queue); got != 3 {
		t.Errorf("queue length after clear = %d; want 3", got)
	}

	p.Stop()
	if state := p.Status().State; state != StateIdle {
		t.Errorf("state after Stop() = %q; want %q", state, StateIdle)
	}
	if got := len(p.Status().Queue); got != 0 {
		t.Errorf("queue length after Stop() = %d; want 0", got)
	}
}

func TestUnplayableTrackIsSkipped(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.wav")
	if err := os.WriteFile(broken, []byte("not audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	good := makeTone(t, dir, "good.wav", 1, 440)

	pub := &fakePublisher{}
	p := newTestPlayer(t, pub)
	p.Enqueue([]jellyfin.Item{
		{ID: broken, Kind: jellyfin.KindTrack, Name: "Broken"},
		{ID: good, Kind: jellyfin.KindTrack, Name: "Good"},
	})

	// The broken track must not wedge the queue: the good one should still play.
	deadline := time.Now().Add(15 * time.Second)
	for pub.count() < 40 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := pub.count(); got < 40 {
		t.Errorf("published %d frames; want the queue to advance past the broken track", got)
	}
}

// TestPlayNowInterruptsAndKeepsQueue pins down the difference between the two
// commands: !play jumps straight to the new selection, while what was already
// queued survives and plays afterwards.
func TestPlayNowInterruptsAndKeepsQueue(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	tone := makeTone(t, dir, "tone.wav", 5, 440)
	item := func(name string) jellyfin.Item {
		return jellyfin.Item{ID: tone, Kind: jellyfin.KindTrack, Name: name}
	}

	p := newTestPlayer(t, &fakePublisher{})
	p.Enqueue([]jellyfin.Item{item("A"), item("B")})
	if current := p.Status().Current; current == nil || current.Name != "A" {
		t.Fatalf("current = %v; want A", current)
	}

	added, truncated := p.PlayNow([]jellyfin.Item{item("X"), item("Y")})
	if added != 2 || truncated {
		t.Fatalf("PlayNow() = %d, %v; want 2, false", added, truncated)
	}

	status := p.Status()
	if status.Current == nil || status.Current.Name != "X" {
		t.Errorf("current = %v; want X to start immediately", status.Current)
	}
	if status.State != StatePlaying {
		t.Errorf("state = %q; want %q", status.State, StatePlaying)
	}

	var names []string
	for _, queued := range status.Queue {
		names = append(names, queued.Name)
	}
	want := []string{"A", "X", "Y", "B"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("queue = %v; want %v — the new items go after the current track, and B is kept", names, want)
	}
}

func TestPlayNowFromIdle(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	track := jellyfin.Item{ID: makeTone(t, dir, "tone.wav", 5, 440), Kind: jellyfin.KindTrack, Name: "Solo"}

	p := newTestPlayer(t, &fakePublisher{})
	if added, _ := p.PlayNow([]jellyfin.Item{track}); added != 1 {
		t.Fatalf("PlayNow() added %d; want 1", added)
	}
	status := p.Status()
	if status.Current == nil || status.Current.Name != "Solo" {
		t.Errorf("current = %v; want Solo", status.Current)
	}
	if len(status.Queue) != 1 {
		t.Errorf("queue length = %d; want 1", len(status.Queue))
	}
}

func TestQueueLimit(t *testing.T) {
	p := New(Options{
		Publisher:  &fakePublisher{},
		FFmpegPath: "ffmpeg",
		MaxQueue:   2,
		Encode:     testEncode,
		URLFor:     func(item jellyfin.Item) string { return "" },
	})
	defer p.Close()

	// Every item here fails to open, which is fine: we only assert the limit.
	added, truncated := p.Enqueue([]jellyfin.Item{
		{ID: "1", Kind: jellyfin.KindTrack}, {ID: "2", Kind: jellyfin.KindTrack}, {ID: "3", Kind: jellyfin.KindTrack},
	})
	if added != 2 || !truncated {
		t.Errorf("Enqueue() = %d, %v; want 2, true", added, truncated)
	}
}

// TestRandomPlaysEveryTrackOnce checks that shuffle works through the queue as
// a bag rather than picking blindly, which would replay tracks while others
// went unheard.
func TestRandomPlaysEveryTrackOnce(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	tone := makeTone(t, dir, "tone.wav", 5, 440)

	var tracks []jellyfin.Item
	for _, name := range []string{"A", "B", "C", "D", "E"} {
		tracks = append(tracks, jellyfin.Item{ID: tone, Kind: jellyfin.KindTrack, Name: name})
	}

	p := newTestPlayer(t, &fakePublisher{})
	p.SetRandom(true)
	p.Enqueue(tracks)

	seen := map[string]int{}
	if current := p.Status().Current; current != nil {
		seen[current.Name]++
	}
	// Four more advances should exhaust the queue exactly.
	for i := 0; i < len(tracks)-1; i++ {
		if !p.Next() {
			t.Fatalf("Next() returned false after %d of %d tracks", i+1, len(tracks))
		}
		if current := p.Status().Current; current != nil {
			seen[current.Name]++
		}
	}
	if len(seen) != len(tracks) {
		t.Errorf("played %d distinct tracks (%v); want all %d before repeating", len(seen), seen, len(tracks))
	}
	// With repeat off the pass ends here.
	if p.Next() {
		t.Error("Next() = true after every track played with repeat off; want false")
	}
}

// TestRandomStartsOnARandomTrack checks that shuffle applies to the track a
// batch opens with, not just to what follows it.
func TestRandomStartsOnARandomTrack(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	tone := makeTone(t, dir, "tone.wav", 5, 440)

	var tracks []jellyfin.Item
	for _, name := range []string{"A", "B", "C", "D", "E", "F", "G", "H"} {
		tracks = append(tracks, jellyfin.Item{ID: tone, Kind: jellyfin.KindTrack, Name: name})
	}

	for _, tc := range []struct {
		name  string
		start func(p *Player, items []jellyfin.Item)
	}{
		{"PlayNow", func(p *Player, items []jellyfin.Item) { p.PlayNow(items) }},
		{"Enqueue", func(p *Player, items []jellyfin.Item) { p.Enqueue(items) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestPlayer(t, &fakePublisher{})
			p.SetRandom(true)

			// Eight tracks over eight passes: always opening on the first one
			// by chance has a probability of 8^-8.
			seen := map[string]int{}
			for i := 0; i < len(tracks); i++ {
				tc.start(p, tracks)
				if current := p.Status().Current; current != nil {
					seen[current.Name]++
				}
				p.Stop()
			}
			if len(seen) == 1 && seen[tracks[0].Name] > 0 {
				t.Errorf("every pass opened on %s; want the first track shuffled too", tracks[0].Name)
			}
			if len(seen) == 0 {
				t.Error("no track ever started playing")
			}
		})
	}
}

func TestRepeatLoopsTheQueue(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	tone := makeTone(t, dir, "tone.wav", 5, 440)
	tracks := []jellyfin.Item{
		{ID: tone, Kind: jellyfin.KindTrack, Name: "A"},
		{ID: tone, Kind: jellyfin.KindTrack, Name: "B"},
	}

	p := newTestPlayer(t, &fakePublisher{})
	p.Enqueue(tracks)
	p.SetRepeat(true)

	if !p.Next() {
		t.Fatal("Next() to the second track failed")
	}
	// At the end of the queue, repeat should wrap to the start.
	if !p.Next() {
		t.Fatal("Next() = false at the end of the queue with repeat on; want it to wrap")
	}
	if current := p.Status().Current; current == nil || current.Name != "A" {
		t.Errorf("after wrapping, current = %v; want A", current)
	}
}

func TestRandomAndRepeatReportedInStatus(t *testing.T) {
	p := newTestPlayer(t, &fakePublisher{})
	if s := p.Status(); s.Random || s.Repeat {
		t.Errorf("defaults = random %v repeat %v; want both off", s.Random, s.Repeat)
	}
	p.SetRandom(true)
	p.SetRepeat(true)
	if s := p.Status(); !s.Random || !s.Repeat {
		t.Errorf("after enabling = random %v repeat %v; want both on", s.Random, s.Repeat)
	}
	p.SetRandom(false)
	if s := p.Status(); s.Random || !s.Repeat {
		t.Errorf("after disabling random = random %v repeat %v; want off/on", s.Random, s.Repeat)
	}
}

// TestStatusIsFreshWhenCommandReturns pins the ordering between a command
// finishing and the status snapshot being updated.
//
// The status used to be published after the caller was released, so a command
// could return while Status() still described the state before it ran. It only
// showed up as a rare CI failure, but the user-visible version is a !next at
// the end of a queue followed by a !nowplaying that still names the old track.
func TestStatusIsFreshWhenCommandReturns(t *testing.T) {
	// No ffmpeg needed: an unplayable URL fails immediately, which is enough to
	// exercise the ordering.
	p := New(Options{
		Publisher:  &fakePublisher{},
		FFmpegPath: "/nonexistent-ffmpeg",
		MaxQueue:   10,
		Encode:     testEncode,
		URLFor:     func(item jellyfin.Item) string { return "" },
	})
	defer p.Close()

	for i := 0; i < 500; i++ {
		p.Enqueue([]jellyfin.Item{{ID: "x", Kind: jellyfin.KindTrack, Name: "X"}})

		p.Stop()
		if got := p.Status(); got.State != StateIdle || len(got.Queue) != 0 {
			t.Fatalf("iteration %d: after Stop() status = %q with %d queued; want idle and empty",
				i, got.State, len(got.Queue))
		}

		p.SetRandom(true)
		if !p.Status().Random {
			t.Fatalf("iteration %d: SetRandom(true) returned before the status showed it", i)
		}
		p.SetRandom(false)
		if p.Status().Random {
			t.Fatalf("iteration %d: SetRandom(false) returned before the status showed it", i)
		}
	}
}

// TestIdlePlayerIsCheap guards the pacing ticker against being left running.
//
// The ticker exists to pace audio at one frame every 20ms. Running it while
// nothing plays wakes the process 50 times a second to do nothing, which
// measured at 0.5% of a core — essentially the entire idle cost of the bot
// sitting in a call. The threshold here is deliberately loose: the difference
// between running and stopped is three orders of magnitude, so this does not
// need to be a precise benchmark to catch a regression.
func TestIdlePlayerIsCheap(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-based")
	}

	p := New(Options{
		Publisher:  &fakePublisher{},
		FFmpegPath: "ffmpeg",
		MaxQueue:   10,
		Encode:     testEncode,
		URLFor:     func(item jellyfin.Item) string { return "" },
	})
	defer p.Close()

	const window = 2 * time.Second
	time.Sleep(200 * time.Millisecond) // let startup settle
	before := processCPU()
	time.Sleep(window)
	used := processCPU() - before

	// A running 20ms ticker costs roughly 10ms of CPU over this window; a
	// stopped one costs microseconds.
	const budget = 3 * time.Millisecond
	if used > budget {
		t.Errorf("idle player used %v CPU over %v (%.2f%% of a core); want under %v — is the pacing ticker still running while idle?",
			used.Round(time.Microsecond), window, float64(used)/float64(window)*100, budget)
	}
}

func processCPU() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// TestTrackChangeEventsDistinguishPauseFromStop pins the contract the in-call
// artwork depends on: pausing keeps the current track, so the tile should keep
// showing its cover, while stopping has no current track and should put up the
// idle card.
func TestTrackChangeEventsDistinguishPauseFromStop(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	track := jellyfin.Item{ID: makeTone(t, dir, "tone.wav", 5, 440), Kind: jellyfin.KindTrack, Name: "Tone"}

	var mu sync.Mutex
	var events []string
	record := func(item *jellyfin.Item) {
		mu.Lock()
		defer mu.Unlock()
		if item == nil {
			events = append(events, "idle")
		} else {
			events = append(events, item.Name)
		}
	}
	seen := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), events...)
	}

	p := New(Options{
		Publisher:    &fakePublisher{},
		FFmpegPath:   "ffmpeg",
		MaxQueue:     10,
		Encode:       testEncode,
		URLFor:       func(item jellyfin.Item) string { return item.ID },
		TrackChanged: record,
	})
	defer p.Close()

	p.Enqueue([]jellyfin.Item{track})
	waitForEvents := func(want int) bool {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if len(seen()) >= want {
				return true
			}
			time.Sleep(10 * time.Millisecond)
		}
		return false
	}
	if !waitForEvents(1) {
		t.Fatalf("no track change on play; got %v", seen())
	}

	p.Pause()
	p.Resume()
	time.Sleep(300 * time.Millisecond)
	if got := seen(); len(got) != 1 {
		t.Errorf("pause/resume emitted %v; want no further events, the track is unchanged", got)
	}

	p.Stop()
	if !waitForEvents(2) {
		t.Fatalf("stop emitted no track change; got %v", seen())
	}
	got := seen()
	if got[len(got)-1] != "idle" {
		t.Errorf("last event = %q; want idle so the tile stops showing the finished track", got[len(got)-1])
	}
}
