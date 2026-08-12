package player

import (
	"sync"
	"testing"
	"time"

	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/jellyfin"
)

// fakeReporter records the lifecycle events the player emits.
type fakeReporter struct {
	mu     sync.Mutex
	events []reportEvent
}

type reportEvent struct {
	kind    string
	item    string
	elapsed time.Duration
	paused  bool
}

func (f *fakeReporter) Started(item jellyfin.Item, elapsed time.Duration) {
	f.record(reportEvent{kind: "started", item: item.Name, elapsed: elapsed})
}

func (f *fakeReporter) Progress(item jellyfin.Item, elapsed time.Duration, paused bool) {
	f.record(reportEvent{kind: "progress", item: item.Name, elapsed: elapsed, paused: paused})
}

func (f *fakeReporter) Stopped(item jellyfin.Item, elapsed time.Duration) {
	f.record(reportEvent{kind: "stopped", item: item.Name, elapsed: elapsed})
}

func (f *fakeReporter) record(e reportEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
}

func (f *fakeReporter) snapshot() []reportEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]reportEvent, len(f.events))
	copy(out, f.events)
	return out
}

// kinds renders the event sequence as "kind:item" for comparison.
func (f *fakeReporter) kinds() []string {
	var out []string
	for _, e := range f.snapshot() {
		out = append(out, e.kind+":"+e.item)
	}
	return out
}

func newReportingPlayer(t *testing.T, rep Reporter, interval time.Duration) *Player {
	t.Helper()
	return New(Options{
		Publisher:        &fakePublisher{},
		FFmpegPath:       "ffmpeg",
		MaxQueue:         100,
		Encode:           testEncode,
		URLFor:           func(item jellyfin.Item) string { return item.ID },
		Reporter:         rep,
		ProgressInterval: interval,
	})
}

// waitFor polls until cond holds, so tests do not depend on how quickly the
// reporting goroutine drains its queue.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", msg)
}

// A track change has to close the previous track's report before opening the
// next one, or the server is left showing two things playing at once.
func TestReporterSeesStartAndStopPerTrack(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	tone := makeTone(t, dir, "tone.wav", 5, 440)

	rep := &fakeReporter{}
	p := newReportingPlayer(t, rep, time.Hour)
	defer p.Close()

	p.Enqueue([]jellyfin.Item{
		{ID: tone, Kind: jellyfin.KindTrack, Name: "A", Duration: 5 * time.Second},
		{ID: tone, Kind: jellyfin.KindTrack, Name: "B", Duration: 5 * time.Second},
	})
	waitFor(t, func() bool { return len(rep.snapshot()) >= 1 }, "the first start")

	if !p.Next() {
		t.Fatal("Next() to the second track failed")
	}
	waitFor(t, func() bool { return len(rep.snapshot()) >= 3 }, "the stop and the second start")

	got := rep.kinds()
	want := []string{"started:A", "stopped:A", "started:B"}
	for i, w := range want {
		if i >= len(got) || got[i] != w {
			t.Fatalf("events = %v; want %v as the first three", got, want)
		}
	}
}

// The stop has to carry where the track actually got to. The core zeroes its
// elapsed time as soon as a track is torn down, so a stop that read it there
// would always report zero.
func TestStopReportsElapsedOfTheTrackThatEnded(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	tone := makeTone(t, dir, "tone.wav", 5, 440)

	rep := &fakeReporter{}
	p := newReportingPlayer(t, rep, time.Hour)
	defer p.Close()

	p.Enqueue([]jellyfin.Item{
		{ID: tone, Kind: jellyfin.KindTrack, Name: "A", Duration: 5 * time.Second},
		{ID: tone, Kind: jellyfin.KindTrack, Name: "B", Duration: 5 * time.Second},
	})
	// Let some audio actually play, so elapsed is meaningfully non-zero.
	waitFor(t, func() bool { return p.Status().Elapsed > 100*time.Millisecond }, "playback to advance")
	p.Next()

	waitFor(t, func() bool {
		for _, e := range rep.snapshot() {
			if e.kind == "stopped" {
				return true
			}
		}
		return false
	}, "a stop")

	for _, e := range rep.snapshot() {
		if e.kind == "stopped" && e.item == "A" {
			if e.elapsed <= 0 {
				t.Errorf("stop for A reported elapsed %v; want the position it reached", e.elapsed)
			}
			return
		}
	}
	t.Error("no stop reported for A")
}

// Pausing is reported as it happens rather than waiting for the next periodic
// update, so the server does not show the bot playing while it sits paused.
func TestPauseAndResumeAreReported(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	tone := makeTone(t, dir, "tone.wav", 5, 440)

	rep := &fakeReporter{}
	p := newReportingPlayer(t, rep, time.Hour)
	defer p.Close()

	p.Enqueue([]jellyfin.Item{{ID: tone, Kind: jellyfin.KindTrack, Name: "A", Duration: 5 * time.Second}})
	waitFor(t, func() bool { return len(rep.snapshot()) >= 1 }, "the start")

	if !p.Pause() {
		t.Fatal("Pause() failed")
	}
	waitFor(t, func() bool {
		for _, e := range rep.snapshot() {
			if e.kind == "progress" && e.paused {
				return true
			}
		}
		return false
	}, "a paused progress report")

	if !p.Resume() {
		t.Fatal("Resume() failed")
	}
	waitFor(t, func() bool {
		seenPaused := false
		for _, e := range rep.snapshot() {
			if e.kind == "progress" && e.paused {
				seenPaused = true
			}
			if seenPaused && e.kind == "progress" && !e.paused {
				return true
			}
		}
		return false
	}, "a resumed progress report")
}

// Progress has to keep flowing on its own, or the server drops the session and
// stops showing what is playing.
func TestProgressIsReportedPeriodically(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	tone := makeTone(t, dir, "tone.wav", 5, 440)

	rep := &fakeReporter{}
	p := newReportingPlayer(t, rep, 20*time.Millisecond)
	defer p.Close()

	p.Enqueue([]jellyfin.Item{{ID: tone, Kind: jellyfin.KindTrack, Name: "A", Duration: 5 * time.Second}})
	waitFor(t, func() bool {
		progress := 0
		for _, e := range rep.snapshot() {
			if e.kind == "progress" {
				progress++
			}
		}
		return progress >= 2
	}, "repeated progress reports")
}

// Shutting down has to close the open playback, or the bot is left in the
// server's now-playing list until the session times out.
func TestCloseReportsAFinalStop(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	tone := makeTone(t, dir, "tone.wav", 5, 440)

	rep := &fakeReporter{}
	p := newReportingPlayer(t, rep, time.Hour)

	p.Enqueue([]jellyfin.Item{{ID: tone, Kind: jellyfin.KindTrack, Name: "A", Duration: 5 * time.Second}})
	waitFor(t, func() bool { return len(rep.snapshot()) >= 1 }, "the start")

	// Close must not return until the stop has been delivered.
	p.Close()

	var stopped bool
	for _, e := range rep.snapshot() {
		if e.kind == "stopped" && e.item == "A" {
			stopped = true
		}
	}
	if !stopped {
		t.Errorf("events after Close() = %v; want a stop for A", rep.kinds())
	}
}

// Stop empties the queue, which has to close the open playback too.
func TestStopCommandReportsAStop(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	tone := makeTone(t, dir, "tone.wav", 5, 440)

	rep := &fakeReporter{}
	p := newReportingPlayer(t, rep, time.Hour)
	defer p.Close()

	p.Enqueue([]jellyfin.Item{{ID: tone, Kind: jellyfin.KindTrack, Name: "A", Duration: 5 * time.Second}})
	waitFor(t, func() bool { return len(rep.snapshot()) >= 1 }, "the start")

	p.Stop()
	waitFor(t, func() bool {
		for _, e := range rep.snapshot() {
			if e.kind == "stopped" {
				return true
			}
		}
		return false
	}, "a stop after Stop()")
}
