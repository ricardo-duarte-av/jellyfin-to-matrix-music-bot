package artwork

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/jellyfin"
)

// fetchSize is the pixel size requested from Jellyfin. Asking for roughly the
// tile size avoids downloading a large cover only to scale it away.
const fetchSize = Size

// Sink is what a rendered H.264 keyframe is handed to; rtc.Publisher satisfies it.
type Sink interface {
	ShowImage(keyframe []byte) error
}

// Library is the subset of the Jellyfin client this needs.
type Library interface {
	Artwork(ctx context.Context, item jellyfin.Item, maxSize int) ([]byte, string, error)
}

// IdleText is what the tile shows when nothing is playing. Leaving the last
// cover up would say the bot is still playing it.
type IdleText struct {
	Title string
	Hint  string
}

// idleKey marks the idle card in the deduplication check.
const idleKey = "idle"

// Publisher keeps the in-call video tile showing the current track's cover.
//
// Rendering shells out to ffmpeg and fetching hits the network, so requests are
// handled on their own goroutine: a track change must never block the player or
// the chat handler. Only the most recent request matters, so a change arriving
// while another is in flight supersedes it.
type Publisher struct {
	renderer *Renderer
	library  Library
	sink     Sink
	idle     IdleText
	log      zerolog.Logger

	mu       sync.Mutex
	pending  *pendingShow
	running  bool
	lastID   string
	closed   bool
	inFlight sync.WaitGroup
}

// NewPublisher builds an artwork publisher.
func NewPublisher(renderer *Renderer, library Library, sink Sink, idle IdleText, log zerolog.Logger) *Publisher {
	return &Publisher{
		renderer: renderer,
		library:  library,
		sink:     sink,
		idle:     idle,
		log:      log.With().Str("component", "artwork").Logger(),
	}
}

// Show updates the tile for item. A nil item means nothing is playing, and puts
// up the idle card.
func (p *Publisher) Show(item *jellyfin.Item) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	// The same tile already on screen needs no work.
	if key := showKey(item); key == p.lastID {
		p.mu.Unlock()
		return
	}
	// Copy, so a caller reusing its item cannot change what is queued here.
	queued := &pendingShow{}
	if item != nil {
		copied := *item
		queued.item = &copied
	}
	p.pending = queued
	if p.running {
		p.mu.Unlock()
		return
	}
	p.running = true
	p.inFlight.Add(1)
	p.mu.Unlock()

	go p.loop()
}

// pendingShow is a queued tile update. A nil item is the idle card.
type pendingShow struct {
	item *jellyfin.Item
}

// loop renders whatever is pending until nothing new has arrived.
func (p *Publisher) loop() {
	defer p.inFlight.Done()
	for {
		p.mu.Lock()
		next := p.pending
		p.pending = nil
		if next == nil || p.closed {
			p.running = false
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()

		if err := p.render(next.item); err != nil {
			p.log.Warn().Err(err).Msg("could not update in-call artwork")
			continue
		}
		p.mu.Lock()
		p.lastID = showKey(next.item)
		p.mu.Unlock()
	}
}

// render fetches the cover and shows it, falling back to a generated tile.
// A nil item renders the idle card.
func (p *Publisher) render(item *jellyfin.Item) error {
	if item == nil {
		frame, err := p.renderer.Placeholder(p.idle.Title, p.idle.Hint)
		if err != nil {
			return err
		}
		return p.sink.ShowImage(frame)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var frame []byte
	cover, _, err := p.library.Artwork(ctx, *item, fetchSize)
	if err == nil {
		frame, err = p.renderer.FromImage(cover)
	}
	if err != nil {
		if err != jellyfin.ErrNoArtwork {
			p.log.Debug().Err(err).Msg("falling back to a generated tile")
		}
		frame, err = p.renderer.Placeholder(item.Name, item.Artist)
		if err != nil {
			return err
		}
	}
	return p.sink.ShowImage(frame)
}

// Close stops accepting updates and waits for any in-flight render.
func (p *Publisher) Close() {
	p.mu.Lock()
	p.closed = true
	p.pending = nil
	p.mu.Unlock()
	p.inFlight.Wait()
}

// showKey identifies the image an item would display, so that consecutive
// tracks from one album do not re-render the same cover.
func showKey(item *jellyfin.Item) string {
	if item == nil {
		return idleKey
	}
	if item.ArtworkID != "" {
		return "art:" + item.ArtworkID
	}
	// Without artwork the tile shows the track's own name, so it is unique
	// per track rather than per album.
	return "text:" + item.Name + "\x00" + item.Artist
}
