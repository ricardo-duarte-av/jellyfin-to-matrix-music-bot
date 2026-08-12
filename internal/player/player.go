// Package player owns the playback queue and the audio pipeline: it pulls
// encoded Opus frames out of ffmpeg and paces them into the LiveKit track.
package player

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/jellyfin"
	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/rtc"
)

// State is what the player is currently doing.
type State string

const (
	StateIdle    State = "idle"
	StatePlaying State = "playing"
	StatePaused  State = "paused"
)

// Status is a snapshot of the player for display commands.
type Status struct {
	State    State
	Current  *jellyfin.Item
	Elapsed  time.Duration
	Queue    []jellyfin.Item
	Position int
	Random   bool
	Repeat   bool
}

// Publisher is the audio sink; rtc.Publisher satisfies it.
type Publisher interface {
	WriteOpus(frame []byte) error
}

// Player runs a single goroutine that owns all queue and playback state.
// Everything else talks to it over channels, so there are no locks around the
// queue itself.
type Player struct {
	pub        Publisher
	ffmpegPath string
	maxQueue   int
	encode     EncodeOptions
	// urlFor resolves a library item to something ffmpeg can read.
	urlFor func(jellyfin.Item) string

	// notices carries playback events (track changes, failures) out to the
	// chat. It is buffered and drained by its own goroutine so that a slow
	// homeserver can never stall the audio loop.
	notices chan string
	// changes carries track-change notifications to the TrackChanged callback.
	changes chan *jellyfin.Item

	cmds chan func(*core)
	stop chan struct{}
	done chan struct{}

	// status is a copy of the state for readers, refreshed by the run loop.
	statusMu sync.RWMutex
	status   Status
}

// core is the state owned exclusively by the run loop.
type core struct {
	queue    []jellyfin.Item
	position int
	state    State

	stream  *stream
	elapsed time.Duration

	// random plays the queue in a shuffled order, repeat loops it once it runs
	// out. Both live only as long as the process, by design.
	random bool
	repeat bool
	// played tracks which queue positions this shuffle pass has used, so
	// random play works through the queue rather than picking blindly and
	// replaying the same track.
	played map[int]bool
	// announced is what the last track-change callback reported, so the run
	// loop can emit one event per actual change.
	announced string
	// advance carries the reason the current track ended.
	skipTo int
}

// Options configures a Player.
type Options struct {
	// Publisher is the audio sink.
	Publisher Publisher
	// FFmpegPath is the ffmpeg binary to run.
	FFmpegPath string
	// MaxQueue caps the queue length.
	MaxQueue int
	// Encode controls the Opus encode.
	Encode EncodeOptions
	// URLFor turns a library item into a URL ffmpeg can read.
	URLFor func(jellyfin.Item) string
	// Notify receives human-readable playback events. Optional.
	Notify func(string)
	// TrackChanged is called with the new current track whenever playback
	// moves, and with nil when playback stops. It gets the item rather than a
	// formatted string so callers can use its artwork. Optional.
	TrackChanged func(*jellyfin.Item)
}

// New creates a player from opts and starts its run loop.
func New(opts Options) *Player {
	p := &Player{
		pub:        opts.Publisher,
		ffmpegPath: opts.FFmpegPath,
		maxQueue:   opts.MaxQueue,
		encode:     opts.Encode,
		urlFor:     opts.URLFor,
		notices:    make(chan string, 32),
		changes:    make(chan *jellyfin.Item, 8),
		cmds:       make(chan func(*core), 16),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
		status:     Status{State: StateIdle},
	}
	go p.run()
	// Both callbacks run on their own goroutines: they reach the network, and
	// the run loop must never wait on the network.
	go func() {
		for msg := range p.notices {
			if opts.Notify != nil {
				opts.Notify(msg)
			}
		}
	}()
	go func() {
		for item := range p.changes {
			if opts.TrackChanged != nil {
				opts.TrackChanged(item)
			}
		}
	}()
	return p
}

// syncTrack emits a track-change event when the current track differs from the
// last one reported. Doing it in one place in the run loop means every path
// that changes tracks -- commands, the queue advancing, failures -- reports
// consistently, rather than each having to remember to.
func (p *Player) syncTrack(c *core) {
	var item *jellyfin.Item
	if c.state != StateIdle {
		item = c.currentItem()
	}
	// Keyed by position too, so repeats of the same track still count.
	key := ""
	if item != nil {
		key = fmt.Sprintf("%d:%s", c.position, item.ID)
	}
	if key == c.announced {
		return
	}
	c.announced = key
	p.announceTrack(item)
}

// announceTrack reports a track change. Unlike notices this must not be
// dropped under load, or the artwork would stop matching what is playing.
func (p *Player) announceTrack(item *jellyfin.Item) {
	select {
	case p.changes <- item:
	case <-p.done:
	}
}

// notify queues a chat notice. It never blocks: if the chat side has fallen
// far behind, the notice is dropped rather than holding up playback.
func (p *Player) notify(msg string) {
	select {
	case p.notices <- msg:
	default:
	}
}

// Close stops playback and shuts the run loop down.
func (p *Player) Close() {
	close(p.stop)
	<-p.done
	close(p.notices)
	close(p.changes)
}

// Status returns the current playback status.
func (p *Player) Status() Status {
	p.statusMu.RLock()
	defer p.statusMu.RUnlock()
	return p.status
}

// do runs fn on the run loop and waits for it.
func (p *Player) do(fn func(*core)) {
	ack := make(chan struct{})
	select {
	case p.cmds <- func(c *core) {
		fn(c)
		close(ack)
	}:
		<-ack
	case <-p.done:
	}
}

// Enqueue appends tracks and starts playing if idle. It returns how many were
// added and whether the queue limit was hit.
func (p *Player) Enqueue(items []jellyfin.Item) (added int, truncated bool) {
	p.do(func(c *core) {
		for _, item := range items {
			if len(c.queue) >= p.maxQueue {
				truncated = true
				break
			}
			c.queue = append(c.queue, item)
			added++
		}
		if c.state == StateIdle && added > 0 {
			c.startAt(p, c.position)
		}
	})
	return added, truncated
}

// PlayNow starts playing items immediately, interrupting whatever is playing.
//
// The items are inserted directly after the current track rather than
// replacing the queue, so anything already lined up still plays afterwards.
// Use Stop or Clear to actually discard a queue.
func (p *Player) PlayNow(items []jellyfin.Item) (added int, truncated bool) {
	p.do(func(c *core) {
		insertAt := c.position
		if c.state != StateIdle {
			insertAt++
		}
		if insertAt > len(c.queue) {
			insertAt = len(c.queue)
		}
		if insertAt < 0 {
			insertAt = 0
		}

		room := p.maxQueue - len(c.queue)
		if room < len(items) {
			truncated = true
			if room < 0 {
				room = 0
			}
			items = items[:room]
		}
		if len(items) == 0 {
			return
		}
		added = len(items)

		// Inserting shifts every later index, so the shuffle bag no longer
		// refers to the tracks it was recorded against.
		c.queue = slices.Insert(c.queue, insertAt, items...)
		c.played = nil
		c.startAt(p, insertAt)
	})
	return added, truncated
}

// SetRandom turns shuffled playback on or off.
func (p *Player) SetRandom(on bool) {
	p.do(func(c *core) {
		c.random = on
		// A fresh shuffle pass, so turning it on mid-queue does not think most
		// of the queue has already been played.
		c.played = nil
	})
}

// SetRepeat turns queue looping on or off.
func (p *Player) SetRepeat(on bool) {
	p.do(func(c *core) { c.repeat = on })
}

// Pause stops feeding audio but keeps the track and ffmpeg alive.
func (p *Player) Pause() (ok bool) {
	p.do(func(c *core) {
		if c.state == StatePlaying {
			c.state = StatePaused
			ok = true
		}
	})
	return ok
}

// Resume continues a paused track.
func (p *Player) Resume() (ok bool) {
	p.do(func(c *core) {
		if c.state == StatePaused {
			c.state = StatePlaying
			ok = true
		}
	})
	return ok
}

// Next skips to the following track, honouring random and repeat.
func (p *Player) Next() (ok bool) {
	p.do(func(c *core) {
		next := c.nextIndex()
		if next < 0 {
			c.stopPlayback()
			c.position = len(c.queue)
			ok = false
			return
		}
		c.startAt(p, next)
		ok = true
	})
	return ok
}

// Prev restarts the previous track.
func (p *Player) Prev() (ok bool) {
	p.do(func(c *core) {
		if c.position <= 0 {
			return
		}
		c.startAt(p, c.position-1)
		ok = true
	})
	return ok
}

// SkipTo jumps to a 1-based queue position.
func (p *Player) SkipTo(oneBased int) (item *jellyfin.Item, err error) {
	p.do(func(c *core) {
		idx := oneBased - 1
		if idx < 0 || idx >= len(c.queue) {
			err = fmt.Errorf("queue position %d is out of range (1-%d)", oneBased, len(c.queue))
			return
		}
		c.startAt(p, idx)
		track := c.queue[idx]
		item = &track
	})
	return item, err
}

// Stop halts playback and clears the queue.
func (p *Player) Stop() {
	p.do(func(c *core) {
		c.stopPlayback()
		c.queue = nil
		c.position = 0
		c.played = nil
	})
}

// Clear empties the queue but leaves the current track playing.
func (p *Player) Clear() (removed int) {
	p.do(func(c *core) {
		if c.state == StateIdle {
			removed = len(c.queue)
			c.queue = nil
			c.position = 0
			c.played = nil
			return
		}
		// Keep everything up to and including the current track.
		removed = len(c.queue) - (c.position + 1)
		if removed < 0 {
			removed = 0
		}
		c.queue = c.queue[:c.position+1]
	})
	return removed
}

// run is the single owner of core state.
func (p *Player) run() {
	defer close(p.done)
	c := &core{state: StateIdle}
	defer c.stopPlayback()

	ticker := time.NewTicker(rtc.FrameDuration)
	defer ticker.Stop()

	for {
		select {
		case <-p.stop:
			return
		case fn := <-p.cmds:
			fn(c)
			p.publishStatus(c)
			p.syncTrack(c)
		case <-ticker.C:
			if c.state != StatePlaying || c.stream == nil {
				continue
			}
			p.pump(c)
			p.syncTrack(c)
		}
	}
}

// pump sends one frame of audio, handling end-of-track.
func (p *Player) pump(c *core) {
	select {
	case frame, ok := <-c.stream.frames:
		if !ok {
			p.trackEnded(c)
			return
		}
		if err := p.pub.WriteOpus(frame.Data); err != nil {
			p.notify(fmt.Sprintf("Playback stopped: %v", err))
			c.stopPlayback()
			return
		}
		c.elapsed += frame.Duration
		p.publishStatus(c)
	default:
		// Buffer underrun: ffmpeg has not kept up. Opus tolerates the gap, so
		// just skip this tick rather than stalling the loop.
	}
}

// trackEnded advances the queue when the current track runs out.
func (p *Player) trackEnded(c *core) {
	err := c.stream.finish()
	current := c.currentItem()
	c.stopPlayback()

	if err != nil && current != nil {
		p.notify(fmt.Sprintf("Skipping %s: %v", current.Describe(), err))
	}
	if next := c.nextIndex(); next >= 0 {
		c.startAt(p, next)
		return
	}
	c.position = len(c.queue)
	p.notify("Queue finished.")
	p.publishStatus(c)
}

// nextIndex picks what to play after the current track, or -1 when the queue
// is done. It is where random and repeat actually take effect.
func (c *core) nextIndex() int {
	if len(c.queue) == 0 {
		return -1
	}
	if !c.random {
		if c.position+1 < len(c.queue) {
			return c.position + 1
		}
		if c.repeat {
			return 0
		}
		return -1
	}

	// Shuffle works through the queue rather than picking blindly, so every
	// track plays once before any repeats.
	candidates := make([]int, 0, len(c.queue))
	for i := range c.queue {
		if !c.played[i] {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		if !c.repeat {
			return -1
		}
		// Start a new pass, avoiding an immediate repeat of the track just
		// finished when there is anything else to choose.
		c.played = nil
		for i := range c.queue {
			if i != c.position || len(c.queue) == 1 {
				candidates = append(candidates, i)
			}
		}
		if len(candidates) == 0 {
			return -1
		}
	}
	return candidates[rand.IntN(len(candidates))]
}

// startAt begins playing queue entry idx, replacing anything already playing.
func (c *core) startAt(p *Player, idx int) {
	c.stopPlayback()
	if idx < 0 || idx >= len(c.queue) {
		c.position = len(c.queue)
		return
	}
	c.position = idx
	if c.played == nil {
		c.played = make(map[int]bool, len(c.queue))
	}
	c.played[idx] = true
	item := c.queue[idx]

	s, err := openStream(p.ffmpegPath, p.urlFor(item), p.encode)
	if err != nil {
		p.notify(fmt.Sprintf("Could not play %s: %v", item.Describe(), err))
		// Move past the broken track rather than wedging the queue.
		if idx+1 < len(c.queue) {
			c.startAt(p, idx+1)
		} else {
			c.position = len(c.queue)
		}
		return
	}
	c.stream = s
	c.elapsed = 0
	c.state = StatePlaying
}

// stopPlayback tears down the current stream and returns to idle. It does not
// announce the change: callers either start another track straight away or
// call announceIdle themselves.
func (c *core) stopPlayback() {
	if c.stream != nil {
		c.stream.Close()
		c.stream = nil
	}
	c.state = StateIdle
	c.elapsed = 0
}

func (c *core) currentItem() *jellyfin.Item {
	if c.position < 0 || c.position >= len(c.queue) {
		return nil
	}
	item := c.queue[c.position]
	return &item
}

func (p *Player) publishStatus(c *core) {
	queue := make([]jellyfin.Item, len(c.queue))
	copy(queue, c.queue)
	p.statusMu.Lock()
	p.status = Status{
		State:    c.state,
		Current:  c.currentItem(),
		Elapsed:  c.elapsed,
		Queue:    queue,
		Position: c.position,
		Random:   c.random,
		Repeat:   c.repeat,
	}
	p.statusMu.Unlock()
}
