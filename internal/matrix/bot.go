// Package matrix implements the chat side of the bot: the sync loop, command
// dispatch, and formatting replies.
package matrix

import (
	"context"
	"fmt"
	"html"
	"strings"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/config"
	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/jellyfin"
	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/player"
)

// Bot wires chat commands to the Jellyfin library and the player.
type Bot struct {
	cfg     *config.Config
	client  *mautrix.Client
	jf      *jellyfin.Client
	player  *player.Player
	results *results
	roomID  id.RoomID
	// startedAt drops events from before the bot came up, so a restart does not
	// replay old commands.
	startedAt time.Time
}

// New builds a bot. The player is supplied separately because it depends on the
// RTC connection, which is established before the sync loop starts.
func New(cfg *config.Config, client *mautrix.Client, jf *jellyfin.Client, plr *player.Player) *Bot {
	return &Bot{
		cfg:       cfg,
		client:    client,
		jf:        jf,
		player:    plr,
		results:   newResults(cfg.Player.ResultTTL),
		roomID:    id.RoomID(cfg.Matrix.RoomID),
		startedAt: time.Now(),
	}
}

// Run syncs until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) error {
	syncer, ok := b.client.Syncer.(*mautrix.DefaultSyncer)
	if !ok {
		return fmt.Errorf("unexpected syncer type %T", b.client.Syncer)
	}
	syncer.OnEventType(event.EventMessage, func(ctx context.Context, evt *event.Event) {
		b.handleMessage(ctx, evt)
	})
	return b.client.SyncWithContext(ctx)
}

// Notify posts a message to the streaming room. It is the player's callback.
func (b *Bot) Notify(msg string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b.send(ctx, msg, "")
}

func (b *Bot) handleMessage(ctx context.Context, evt *event.Event) {
	if evt.RoomID != b.roomID || evt.Sender == b.client.UserID {
		return
	}
	if evt.Timestamp < b.startedAt.UnixMilli() {
		return
	}
	content, ok := evt.Content.Parsed.(*event.MessageEventContent)
	if !ok || content.MsgType != event.MsgText {
		return
	}
	cmd, ok := ParseCommand(b.cfg.Matrix.CommandPrefix, content.Body)
	if !ok {
		return
	}
	b.dispatch(ctx, evt, cmd)
}

// controlCommands are gated behind the admin list because they affect what
// everyone in the call hears.
var controlCommands = map[string]bool{
	"stop":  true,
	"clear": true,
	"skip":  true,
}

func (b *Bot) dispatch(ctx context.Context, evt *event.Event, cmd Command) {
	name := canonicalName(cmd.Name)
	if controlCommands[name] && !b.cfg.IsAdmin(evt.Sender.String()) {
		b.reply(ctx, fmt.Sprintf("Sorry, %s is limited to admins.", b.cfg.Matrix.CommandPrefix+name), "")
		return
	}

	switch name {
	case "help":
		b.cmdHelp(ctx)
	case "search":
		b.cmdSearch(ctx, cmd)
	case "list":
		b.cmdList(ctx)
	case "play":
		b.cmdPlay(ctx, cmd)
	case "queue":
		b.cmdQueue(ctx, cmd)
	case "nowplaying":
		b.cmdNowPlaying(ctx)
	case "pause":
		if b.player.Pause() {
			b.reply(ctx, "Paused.", "")
		} else {
			b.reply(ctx, "Nothing is playing.", "")
		}
	case "resume":
		if b.player.Resume() {
			b.reply(ctx, "Resumed.", "")
		} else {
			b.reply(ctx, "Nothing is paused.", "")
		}
	case "next":
		if b.player.Next() {
			b.replyNowPlaying(ctx)
		} else {
			b.reply(ctx, "Nothing left in the queue.", "")
		}
	case "prev":
		if b.player.Prev() {
			b.replyNowPlaying(ctx)
		} else {
			b.reply(ctx, "Already at the start of the queue.", "")
		}
	case "skip":
		b.cmdSkip(ctx, cmd)
	case "clear":
		removed := b.player.Clear()
		b.reply(ctx, fmt.Sprintf("Cleared %d queued track(s).", removed), "")
	case "stop":
		b.player.Stop()
		b.reply(ctx, "Stopped and cleared the queue.", "")
	default:
		b.reply(ctx, fmt.Sprintf("Unknown command %q. Try %shelp.", cmd.Name, b.cfg.Matrix.CommandPrefix), "")
	}
}

func (b *Bot) cmdHelp(ctx context.Context) {
	p := b.cfg.Matrix.CommandPrefix
	lines := [][2]string{
		{p + "search [artist|album|track|playlist] <query>", "search the library; results are numbered"},
		{p + "list", "show the last search results again"},
		{p + "play <n> [n ...]", "play results by number right away"},
		{p + "play <query>", "search and play the best match right away"},
		{p + "queue <n> | <query>", "add to the end of the queue instead"},
		{p + "queue", "show the queue"},
		{p + "nowplaying", "show the current track"},
		{p + "pause / " + p + "resume", "pause or resume playback"},
		{p + "next / " + p + "prev", "move through the queue"},
		{p + "skip <n>", "jump to queue position n"},
		{p + "clear", "drop everything after the current track"},
		{p + "stop", "stop playing and empty the queue"},
	}

	var plain, htmlOut strings.Builder
	plain.WriteString("Commands:\n")
	htmlOut.WriteString("<b>Commands</b><ul>")
	for _, line := range lines {
		fmt.Fprintf(&plain, "  %s — %s\n", line[0], line[1])
		fmt.Fprintf(&htmlOut, "<li><code>%s</code> — %s</li>", html.EscapeString(line[0]), html.EscapeString(line[1]))
	}
	htmlOut.WriteString("</ul>")
	b.reply(ctx, strings.TrimRight(plain.String(), "\n"), htmlOut.String())
}

func (b *Bot) cmdSearch(ctx context.Context, cmd Command) {
	if cmd.Rest == "" {
		b.reply(ctx, "Usage: "+b.cfg.Matrix.CommandPrefix+"search [artist|album|track|playlist] <query>", "")
		return
	}

	kinds := jellyfin.AllKinds
	query := cmd.Rest
	if len(cmd.Args) > 1 {
		if kind, ok := jellyfin.ParseKind(cmd.Args[0]); ok {
			kinds = []jellyfin.Kind{kind}
			query = strings.TrimSpace(strings.TrimPrefix(cmd.Rest, cmd.Args[0]))
		}
	}

	items, err := b.jf.Search(ctx, kinds, query, b.cfg.Player.SearchLimit)
	if err != nil {
		b.reply(ctx, "Search failed: "+err.Error(), "")
		return
	}
	if len(items) == 0 {
		b.reply(ctx, fmt.Sprintf("Nothing found for %q.", query), "")
		return
	}
	b.results.set(items)
	b.replyResults(ctx, fmt.Sprintf("Results for %q", query), items)
}

func (b *Bot) cmdList(ctx context.Context) {
	items := b.results.get()
	if len(items) == 0 {
		b.reply(ctx, "No recent search results — run a search first.", "")
		return
	}
	b.replyResults(ctx, "Last search results", items)
}

// cmdPlay starts playing the requested items right away. Anything already
// queued still plays afterwards.
func (b *Bot) cmdPlay(ctx context.Context, cmd Command) {
	if cmd.Rest == "" {
		b.reply(ctx, "Usage: "+b.cfg.Matrix.CommandPrefix+"play <number> or "+b.cfg.Matrix.CommandPrefix+"play <query>", "")
		return
	}
	tracks, ok := b.resolveTracks(ctx, cmd)
	if !ok || len(tracks) == 0 {
		return
	}

	added, truncated := b.player.PlayNow(tracks)
	msg := "Now playing: " + tracks[0].Describe()
	if added > 1 {
		msg += fmt.Sprintf(" (+%d more)", added-1)
	}
	if truncated {
		msg += fmt.Sprintf(" Queue limit of %d reached.", b.cfg.Player.MaxQueue)
	}
	b.reply(ctx, msg, "")
}

// cmdQueue appends to the queue, or shows it when given no arguments.
func (b *Bot) cmdQueue(ctx context.Context, cmd Command) {
	if cmd.Rest == "" {
		b.showQueue(ctx)
		return
	}
	tracks, ok := b.resolveTracks(ctx, cmd)
	if !ok || len(tracks) == 0 {
		return
	}

	wasIdle := b.player.Status().State == player.StateIdle
	added, truncated := b.player.Enqueue(tracks)

	msg := fmt.Sprintf("Queued %d track(s).", added)
	if added == 1 {
		msg = "Queued " + tracks[0].Describe() + "."
	}
	if truncated {
		msg += fmt.Sprintf(" Queue limit of %d reached.", b.cfg.Player.MaxQueue)
	}
	if wasIdle {
		if current := b.player.Status().Current; current != nil {
			msg += " Now playing: " + current.Describe()
		}
	}
	b.reply(ctx, msg, "")
}

// resolveTracks turns "<number> [number ...]" or "<query>" into playable
// tracks, expanding artists, albums and playlists. It reports any problem to
// the room itself and returns false when nothing should be played.
func (b *Bot) resolveTracks(ctx context.Context, cmd Command) ([]jellyfin.Item, bool) {
	var picked []jellyfin.Item
	if indexes, ok := intArgs(cmd.Args); ok {
		for _, index := range indexes {
			item, err := b.results.resolve(index)
			if err != nil {
				b.reply(ctx, err.Error(), "")
				return nil, false
			}
			picked = append(picked, item)
		}
	} else {
		// Treat the argument as a search and take the best match, preferring a
		// track so "!play some song" does not pull in a whole artist.
		items, err := b.jf.Search(ctx, jellyfin.AllKinds, cmd.Rest, b.cfg.Player.SearchLimit)
		if err != nil {
			b.reply(ctx, "Search failed: "+err.Error(), "")
			return nil, false
		}
		if len(items) == 0 {
			b.reply(ctx, fmt.Sprintf("Nothing found for %q.", cmd.Rest), "")
			return nil, false
		}
		b.results.set(items)
		picked = append(picked, bestMatch(items))
	}

	var tracks []jellyfin.Item
	for _, item := range picked {
		expanded, err := b.jf.Tracks(ctx, item)
		if err != nil {
			b.reply(ctx, fmt.Sprintf("Could not expand %s: %v", item.Describe(), err), "")
			return nil, false
		}
		if len(expanded) == 0 {
			b.reply(ctx, fmt.Sprintf("%s has no playable tracks.", item.Describe()), "")
			continue
		}
		tracks = append(tracks, expanded...)
	}
	if len(tracks) == 0 {
		b.reply(ctx, "Nothing playable in that selection.", "")
		return nil, false
	}
	return tracks, true
}

func (b *Bot) cmdSkip(ctx context.Context, cmd Command) {
	indexes, ok := intArgs(cmd.Args)
	if !ok || len(indexes) != 1 {
		b.reply(ctx, "Usage: "+b.cfg.Matrix.CommandPrefix+"skip <queue position>", "")
		return
	}
	item, err := b.player.SkipTo(indexes[0])
	if err != nil {
		b.reply(ctx, err.Error(), "")
		return
	}
	b.reply(ctx, "Now playing: "+item.Describe(), "")
}

func (b *Bot) showQueue(ctx context.Context) {
	status := b.player.Status()
	if len(status.Queue) == 0 {
		b.reply(ctx, "The queue is empty.", "")
		return
	}

	var plain, htmlOut strings.Builder
	plain.WriteString("Queue:\n")
	htmlOut.WriteString("<b>Queue</b><ol>")
	for i, item := range status.Queue {
		marker := ""
		if i == status.Position && status.State != player.StateIdle {
			marker = " ← now playing"
		}
		fmt.Fprintf(&plain, "  %d. %s%s\n", i+1, item.Describe(), marker)
		if marker != "" {
			fmt.Fprintf(&htmlOut, "<li><b>%s</b> ← now playing</li>", html.EscapeString(item.Describe()))
		} else {
			fmt.Fprintf(&htmlOut, "<li>%s</li>", html.EscapeString(item.Describe()))
		}
	}
	htmlOut.WriteString("</ol>")
	b.reply(ctx, strings.TrimRight(plain.String(), "\n"), htmlOut.String())
}

func (b *Bot) cmdNowPlaying(ctx context.Context) {
	b.replyNowPlaying(ctx)
}

func (b *Bot) replyNowPlaying(ctx context.Context) {
	status := b.player.Status()
	if status.Current == nil || status.State == player.StateIdle {
		b.reply(ctx, "Nothing is playing.", "")
		return
	}
	line := status.Current.Describe()
	position := fmt.Sprintf(" [%s", jellyfin.FormatDuration(status.Elapsed))
	if status.Current.Duration > 0 {
		position += "/" + jellyfin.FormatDuration(status.Current.Duration)
	}
	position += "]"

	state := ""
	if status.State == player.StatePaused {
		state = " (paused)"
	}
	b.reply(ctx, "Now playing: "+line+position+state, "")
}

func (b *Bot) replyResults(ctx context.Context, title string, items []jellyfin.Item) {
	var plain, htmlOut strings.Builder
	fmt.Fprintf(&plain, "%s:\n", title)
	fmt.Fprintf(&htmlOut, "<b>%s</b><ol>", html.EscapeString(title))
	for i, item := range items {
		fmt.Fprintf(&plain, "  %d. [%s] %s\n", i+1, item.Kind, item.Describe())
		fmt.Fprintf(&htmlOut, "<li><i>%s</i> %s</li>", html.EscapeString(string(item.Kind)), html.EscapeString(item.Describe()))
	}
	htmlOut.WriteString("</ol>")
	prefix := b.cfg.Matrix.CommandPrefix
	fmt.Fprintf(&plain, "%splay <number> to play now, %squeue <number> to add to the queue", prefix, prefix)
	fmt.Fprintf(&htmlOut, "<code>%splay &lt;number&gt;</code> to play now, <code>%squeue &lt;number&gt;</code> to add to the queue",
		html.EscapeString(prefix), html.EscapeString(prefix))
	b.reply(ctx, plain.String(), htmlOut.String())
}

func (b *Bot) reply(ctx context.Context, plain, htmlBody string) {
	b.send(ctx, plain, htmlBody)
}

func (b *Bot) send(ctx context.Context, plain, htmlBody string) {
	content := event.MessageEventContent{
		MsgType: event.MsgNotice,
		Body:    plain,
	}
	if htmlBody != "" {
		content.Format = event.FormatHTML
		content.FormattedBody = htmlBody
	}
	if _, err := b.client.SendMessageEvent(ctx, b.roomID, event.EventMessage, &content); err != nil {
		b.client.Log.Err(err).Msg("failed to send message")
	}
}

// bestMatch picks what a bare "!play <query>" should start. A track is the
// least surprising choice; otherwise fall back to whatever came first.
func bestMatch(items []jellyfin.Item) jellyfin.Item {
	for _, item := range items {
		if item.Kind == jellyfin.KindTrack {
			return item
		}
	}
	return items[0]
}
