package artwork

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/jellyfin"
)

type fakeSink struct {
	mu     sync.Mutex
	frames [][]byte
}

func (s *fakeSink) ShowImage(frame []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, frame)
	return nil
}

func (s *fakeSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.frames)
}

type fakeLibrary struct {
	mu    sync.Mutex
	cover []byte
	err   error
	calls int
}

func (l *fakeLibrary) Artwork(ctx context.Context, item jellyfin.Item, maxSize int) ([]byte, string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.err != nil {
		return nil, "", l.err
	}
	return l.cover, "image/jpeg", nil
}

func (l *fakeLibrary) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

func newTestPublisher(t *testing.T, lib Library) (*Publisher, *fakeSink) {
	t.Helper()
	sink := &fakeSink{}
	p := NewPublisher(NewRenderer("ffmpeg"), lib, sink, zerolog.New(io.Discard))
	t.Cleanup(p.Close)
	return p, sink
}

// waitFor polls until cond holds or the deadline passes.
func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestPublisherShowsCover(t *testing.T) {
	requireFFmpeg(t)
	lib := &fakeLibrary{cover: makeCover(t)}
	p, sink := newTestPublisher(t, lib)

	p.Show(&jellyfin.Item{ID: "t1", Name: "Track", Artist: "Artist", ArtworkID: "album1"})
	if !waitFor(t, func() bool { return sink.count() == 1 }) {
		t.Fatalf("no frame published; got %d", sink.count())
	}
}

// Consecutive tracks from one album share a cover, so the tile should not be
// re-rendered for each of them.
func TestPublisherSkipsRepeatedArtwork(t *testing.T) {
	requireFFmpeg(t)
	lib := &fakeLibrary{cover: makeCover(t)}
	p, sink := newTestPublisher(t, lib)

	p.Show(&jellyfin.Item{ID: "t1", Name: "One", ArtworkID: "album1"})
	if !waitFor(t, func() bool { return sink.count() == 1 }) {
		t.Fatal("first cover never published")
	}
	for _, id := range []string{"t2", "t3", "t4"} {
		p.Show(&jellyfin.Item{ID: id, Name: id, ArtworkID: "album1"})
	}
	time.Sleep(300 * time.Millisecond)

	if got := sink.count(); got != 1 {
		t.Errorf("published %d frames for one album cover; want 1", got)
	}
	if got := lib.callCount(); got != 1 {
		t.Errorf("fetched artwork %d times; want 1", got)
	}
}

// A track with no cover still gets a tile, generated from its name.
func TestPublisherFallsBackToPlaceholder(t *testing.T) {
	requireFFmpeg(t)
	lib := &fakeLibrary{err: jellyfin.ErrNoArtwork}
	p, sink := newTestPublisher(t, lib)

	p.Show(&jellyfin.Item{ID: "t1", Name: "Nameless Track", Artist: "Someone"})
	if !waitFor(t, func() bool { return sink.count() == 1 }) {
		t.Fatal("no placeholder published")
	}
}

// A cover that fails to decode must not leave the tile blank.
func TestPublisherFallsBackWhenRenderFails(t *testing.T) {
	requireFFmpeg(t)
	lib := &fakeLibrary{cover: []byte("not an image")}
	p, sink := newTestPublisher(t, lib)

	p.Show(&jellyfin.Item{ID: "t1", Name: "Broken Cover", Artist: "Someone", ArtworkID: "album1"})
	if !waitFor(t, func() bool { return sink.count() == 1 }) {
		t.Fatal("no fallback tile published after a render failure")
	}
}

func TestPublisherIgnoresNilItem(t *testing.T) {
	requireFFmpeg(t)
	p, sink := newTestPublisher(t, &fakeLibrary{cover: makeCover(t)})

	p.Show(nil)
	time.Sleep(200 * time.Millisecond)
	if got := sink.count(); got != 0 {
		t.Errorf("published %d frames for a nil item; want the last cover left in place", got)
	}
}

// Rapid skipping must not queue up a render per skip.
func TestPublisherCoalescesRapidChanges(t *testing.T) {
	requireFFmpeg(t)
	lib := &fakeLibrary{cover: makeCover(t)}
	p, sink := newTestPublisher(t, lib)

	for i := 0; i < 20; i++ {
		p.Show(&jellyfin.Item{ID: string(rune('a' + i)), Name: "Track", ArtworkID: string(rune('A' + i))})
	}
	if !waitFor(t, func() bool { return sink.count() > 0 }) {
		t.Fatal("nothing published")
	}
	time.Sleep(500 * time.Millisecond)

	// The first and the last are what matter; everything in between is stale
	// by the time it could be rendered.
	if got := sink.count(); got > 5 {
		t.Errorf("rendered %d frames for 20 rapid changes; expected them to coalesce", got)
	}
}
