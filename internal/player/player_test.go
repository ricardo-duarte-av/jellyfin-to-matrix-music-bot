package player

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
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

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
}

func newTestPlayer(t *testing.T, pub Publisher) *Player {
	t.Helper()
	p := New(pub, "ffmpeg", 100, func(item jellyfin.Item) string { return item.ID }, nil)
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

func TestQueueLimit(t *testing.T) {
	p := New(&fakePublisher{}, "ffmpeg", 2, func(item jellyfin.Item) string { return "" }, nil)
	defer p.Close()

	// Every item here fails to open, which is fine: we only assert the limit.
	added, truncated := p.Enqueue([]jellyfin.Item{
		{ID: "1", Kind: jellyfin.KindTrack}, {ID: "2", Kind: jellyfin.KindTrack}, {ID: "3", Kind: jellyfin.KindTrack},
	})
	if added != 2 || !truncated {
		t.Errorf("Enqueue() = %d, %v; want 2, true", added, truncated)
	}
}
