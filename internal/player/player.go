// Package player owns the playback queue and the audio pipeline: it pulls
// encoded Opus frames out of ffmpeg and paces them into the LiveKit track.
package player

import (
	"fmt"
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
	// urlFor resolves a library item to something ffmpeg can read.
	urlFor func(jellyfin.Item) string

	// notices carries playback events (track changes, failures) out to the
	// chat. It is buffered and drained by its own goroutine so that a slow
	// homeserver can never stall the audio loop.
	notices chan string

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
	// advance carries the reason the current track ended.
	skipTo int
}

// New creates a player writing Opus to pub. urlFor turns a library item into a
// URL ffmpeg can read; notify receives playback events and may be nil.
func New(pub Publisher, ffmpegPath string, maxQueue int, urlFor func(jellyfin.Item) string, notify func(string)) *Player {
	p := &Player{
		pub:        pub,
		ffmpegPath: ffmpegPath,
		maxQueue:   maxQueue,
		urlFor:     urlFor,
		notices:    make(chan string, 32),
		cmds:       make(chan func(*core), 16),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
		status:     Status{State: StateIdle},
	}
	go p.run()
	go func() {
		for msg := range p.notices {
			if notify != nil {
				notify(msg)
			}
		}
	}()
	return p
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

		c.queue = slices.Insert(c.queue, insertAt, items...)
		c.startAt(p, insertAt)
	})
	return added, truncated
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

// Next skips to the following track.
func (p *Player) Next() (ok bool) {
	p.do(func(c *core) {
		if c.position+1 >= len(c.queue) {
			c.stopPlayback()
			c.position = len(c.queue)
			ok = false
			return
		}
		c.startAt(p, c.position+1)
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
	})
}

// Clear empties the queue but leaves the current track playing.
func (p *Player) Clear() (removed int) {
	p.do(func(c *core) {
		if c.state == StateIdle {
			removed = len(c.queue)
			c.queue = nil
			c.position = 0
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
		case <-ticker.C:
			if c.state != StatePlaying || c.stream == nil {
				continue
			}
			p.pump(c)
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
	if c.position+1 < len(c.queue) {
		c.startAt(p, c.position+1)
		if item := c.currentItem(); item != nil {
			p.notify("Now playing: " + item.Describe())
		}
		return
	}
	c.position = len(c.queue)
	p.notify("Queue finished.")
	p.publishStatus(c)
}

// startAt begins playing queue entry idx, replacing anything already playing.
func (c *core) startAt(p *Player, idx int) {
	c.stopPlayback()
	if idx < 0 || idx >= len(c.queue) {
		c.position = len(c.queue)
		return
	}
	c.position = idx
	item := c.queue[idx]

	s, err := openStream(p.ffmpegPath, p.urlFor(item))
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

// stopPlayback tears down the current stream and returns to idle.
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
	}
	p.statusMu.Unlock()
}
