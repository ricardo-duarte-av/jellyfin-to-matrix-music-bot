package jellyfin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"
	api "github.com/sj14/jellyfin-go/api"
)

// ticksPerSecond is Jellyfin's time unit: 100ns ticks.
const ticksPerSecond = int64(time.Second / (100 * time.Nanosecond))

// playedFraction is how much of a track must play before it counts as played.
// It matches what Jellyfin's own clients use, so the bot's play counts line up
// with everything else in the library.
const playedFraction = 0.9

// progressInterval is how often playback progress is reported. Jellyfin's own
// clients use ten seconds; the session goes stale without these.
const ProgressInterval = 10 * time.Second

// ticks converts a duration to Jellyfin ticks.
func ticks(d time.Duration) int64 {
	if d < 0 {
		return 0
	}
	return int64(d / (100 * time.Nanosecond))
}

// ReportStart tells Jellyfin the bot has begun playing an item. playSessionID
// ties the start, its progress reports and the eventual stop together.
func (c *Client) ReportStart(ctx context.Context, item Item, position time.Duration, playSessionID string) error {
	info := api.NewPlaybackStartInfo()
	info.SetItemId(item.ID)
	info.SetMediaSourceId(item.ID)
	info.SetPlaySessionId(playSessionID)
	// The bot hands the original file to ffmpeg (see StreamURL) rather than
	// asking Jellyfin to transcode, so as far as the server is concerned this
	// is a direct stream.
	info.SetPlayMethod(api.PLAYMETHOD_DIRECT_STREAM)
	// There is no seek command, and the queue is the bot's own.
	info.SetCanSeek(false)
	info.SetIsPaused(false)
	info.SetPositionTicks(ticks(position))

	if _, err := c.api.PlaystateAPI.ReportPlaybackStart(ctx).PlaybackStartInfo(*info).Execute(); err != nil {
		return fmt.Errorf("report playback start: %w", err)
	}
	return nil
}

// ReportProgress updates Jellyfin's idea of where the bot is in a track.
func (c *Client) ReportProgress(ctx context.Context, item Item, position time.Duration, paused bool, playSessionID string) error {
	info := api.NewPlaybackProgressInfo()
	info.SetItemId(item.ID)
	info.SetMediaSourceId(item.ID)
	info.SetPlaySessionId(playSessionID)
	info.SetPlayMethod(api.PLAYMETHOD_DIRECT_STREAM)
	info.SetCanSeek(false)
	info.SetIsPaused(paused)
	info.SetPositionTicks(ticks(position))

	if _, err := c.api.PlaystateAPI.ReportPlaybackProgress(ctx).PlaybackProgressInfo(*info).Execute(); err != nil {
		return fmt.Errorf("report playback progress: %w", err)
	}
	return nil
}

// ReportStopped tells Jellyfin playback of an item has ended.
func (c *Client) ReportStopped(ctx context.Context, item Item, position time.Duration, playSessionID string) error {
	info := api.NewPlaybackStopInfo()
	info.SetItemId(item.ID)
	info.SetMediaSourceId(item.ID)
	info.SetPlaySessionId(playSessionID)
	info.SetPositionTicks(ticks(position))

	if _, err := c.api.PlaystateAPI.ReportPlaybackStopped(ctx).PlaybackStopInfo(*info).Execute(); err != nil {
		return fmt.Errorf("report playback stopped: %w", err)
	}
	return nil
}

// MarkPlayed records a play against the configured user.
//
// The session reports above cannot do this on their own: they credit the user
// behind the session, and an API key authenticates as the server with no user
// attached, so nothing would ever reach a play count. This endpoint takes the
// user explicitly, which is why play counts work at all.
func (c *Client) MarkPlayed(ctx context.Context, item Item) error {
	_, _, err := c.api.PlaystateAPI.MarkPlayedItem(ctx, item.ID).
		UserId(c.userID).
		DatePlayed(time.Now().UTC()).
		Execute()
	if err != nil {
		return fmt.Errorf("mark played: %w", err)
	}
	return nil
}

// PlaybackReporter feeds playback events back to Jellyfin. It satisfies
// player.Reporter, keeping contexts, HTTP and error handling out of the
// player's audio loop.
//
// Every method is called from a goroutine the player owns, one event at a
// time, so the only shared state that needs guarding is the play session.
type PlaybackReporter struct {
	client  *Client
	log     zerolog.Logger
	timeout time.Duration

	mu sync.Mutex
	// session identifies the current track's playback to Jellyfin. Start,
	// progress and stop for one track must all carry the same value.
	session string
	// startedID is the item the current session belongs to, so a stop for
	// anything else can be ignored rather than corrupting the session.
	startedID string
}

// reportTimeout caps how long one report may take. Reporting is bookkeeping:
// if the server is slow, giving up is better than queueing events behind it.
const reportTimeout = 15 * time.Second

// NewPlaybackReporter builds a reporter that posts to Jellyfin.
func NewPlaybackReporter(client *Client, log zerolog.Logger) *PlaybackReporter {
	return &PlaybackReporter{client: client, log: log, timeout: reportTimeout}
}

// Started reports a track beginning.
func (r *PlaybackReporter) Started(item Item, elapsed time.Duration) {
	r.mu.Lock()
	r.session = newPlaySessionID()
	r.startedID = item.ID
	session := r.session
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	if err := r.client.ReportStart(ctx, item, elapsed, session); err != nil {
		r.log.Warn().Err(err).Str("item", item.Name).Msg("could not report playback start to jellyfin")
	}
}

// Progress reports where the current track is, and whether it is paused.
func (r *PlaybackReporter) Progress(item Item, elapsed time.Duration, paused bool) {
	session, ok := r.sessionFor(item)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	if err := r.client.ReportProgress(ctx, item, elapsed, paused, session); err != nil {
		r.log.Debug().Err(err).Str("item", item.Name).Msg("could not report playback progress to jellyfin")
	}
}

// Stopped reports a track ending, and records a play when enough of it ran.
func (r *PlaybackReporter) Stopped(item Item, elapsed time.Duration) {
	session, ok := r.sessionFor(item)
	if !ok {
		return
	}
	r.mu.Lock()
	r.session = ""
	r.startedID = ""
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	if err := r.client.ReportStopped(ctx, item, elapsed, session); err != nil {
		r.log.Warn().Err(err).Str("item", item.Name).Msg("could not report playback stop to jellyfin")
	}
	if !CountsAsPlayed(item, elapsed) {
		return
	}
	if err := r.client.MarkPlayed(ctx, item); err != nil {
		r.log.Warn().Err(err).Str("item", item.Name).Msg("could not mark item played in jellyfin")
	}
}

// sessionFor returns the play session for item, or false when the item is not
// the one currently being reported — a stale progress tick for a track that
// already ended, say.
func (r *PlaybackReporter) sessionFor(item Item) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.session == "" || r.startedID != item.ID {
		return "", false
	}
	return r.session, true
}

// CountsAsPlayed reports whether listening to elapsed of item is enough to
// record a play. A track of unknown length counts once anything played, since
// there is no total to measure against.
func CountsAsPlayed(item Item, elapsed time.Duration) bool {
	if elapsed <= 0 {
		return false
	}
	if item.Duration <= 0 {
		return true
	}
	return float64(elapsed) >= playedFraction*float64(item.Duration)
}

// newPlaySessionID returns an opaque identifier for one track's playback.
func newPlaySessionID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// Only reachable if the system entropy source fails. A timestamp still
		// distinguishes consecutive tracks, which is all this needs to do.
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}
