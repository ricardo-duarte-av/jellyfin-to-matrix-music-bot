package player

import (
	"fmt"
	"time"

	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/jellyfin"
)

// Reporter receives playback lifecycle events so they can be reported to the
// library server. jellyfin.PlaybackReporter satisfies it.
//
// Every method is called from a goroutine the player owns, one event at a time
// and in order, so implementations may block on the network without affecting
// playback.
type Reporter interface {
	// Started is called when a track begins.
	Started(item jellyfin.Item, elapsed time.Duration)
	// Progress is called periodically while a track plays, and whenever it is
	// paused or resumed.
	Progress(item jellyfin.Item, elapsed time.Duration, paused bool)
	// Stopped is called when a track ends, whatever the reason: it finished,
	// it was skipped, or playback stopped.
	Stopped(item jellyfin.Item, elapsed time.Duration)
}

// reportKind is which lifecycle event a report carries.
type reportKind int

const (
	reportStarted reportKind = iota
	reportProgress
	reportStopped
)

// report is one queued lifecycle event.
type report struct {
	kind    reportKind
	item    jellyfin.Item
	elapsed time.Duration
	paused  bool
}

// reportState is what the reporter has been told so far, so the run loop can
// emit one event per real change.
//
// It carries its own copy of the item and elapsed time because the core's are
// reset the moment a track is torn down, and a stop has to report where the
// track that just ended actually got to.
type reportState struct {
	key     string
	item    jellyfin.Item
	elapsed time.Duration
	paused  bool
}

// syncReport turns the current state into lifecycle events. It mirrors
// syncTrack: one place decides what changed, so every path through the queue
// reports consistently.
func (p *Player) syncReport(c *core) {
	var item *jellyfin.Item
	if c.state != StateIdle {
		item = c.currentItem()
	}
	// Keyed by position too, so a track repeating counts as a new play.
	key := ""
	if item != nil {
		key = fmt.Sprintf("%d:%s", c.position, item.ID)
	}

	if key != c.rep.key {
		if c.rep.key != "" {
			// Losing a stop would leave Jellyfin showing the bot playing a track
			// it finished long ago, so this one waits its turn.
			p.emitReport(report{kind: reportStopped, item: c.rep.item, elapsed: c.rep.elapsed}, true)
		}
		c.rep = reportState{key: key}
		if item != nil {
			c.rep.item = *item
			c.rep.elapsed = c.elapsed
			c.rep.paused = c.state == StatePaused
			p.emitReport(report{kind: reportStarted, item: *item, elapsed: c.elapsed}, true)
		}
		return
	}
	if key == "" {
		return
	}

	// Same track: keep the elapsed time current so the eventual stop is
	// accurate, and report pausing and resuming as they happen.
	c.rep.elapsed = c.elapsed
	if paused := c.state == StatePaused; paused != c.rep.paused {
		c.rep.paused = paused
		p.emitReport(report{kind: reportProgress, item: c.rep.item, elapsed: c.elapsed, paused: paused}, false)
	}
}

// tickReport emits the periodic progress update that keeps the server's session
// alive.
func (p *Player) tickReport(c *core) {
	if c.rep.key == "" {
		return
	}
	p.emitReport(report{
		kind:    reportProgress,
		item:    c.rep.item,
		elapsed: c.rep.elapsed,
		paused:  c.rep.paused,
	}, false)
}

// finalReport stops whatever the reporter still thinks is playing. Without it a
// shutdown would leave the track hanging in the server's now-playing list.
func (p *Player) finalReport(c *core) {
	if c.rep.key == "" {
		return
	}
	p.emitReport(report{kind: reportStopped, item: c.rep.item, elapsed: c.rep.elapsed}, true)
	c.rep = reportState{}
}

// emitReport queues a report. Progress updates are dropped when the reporter
// has fallen behind — another one follows shortly — but starts and stops are
// waited on, since they are what the server's state is built from.
func (p *Player) emitReport(r report, mustDeliver bool) {
	if p.reports == nil {
		return
	}
	if !mustDeliver {
		select {
		case p.reports <- r:
		default:
		}
		return
	}
	select {
	case p.reports <- r:
	case <-p.done:
	}
}
