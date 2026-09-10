// Command musicbot streams music from a Jellyfin server into a Matrix room's
// Element Call, driven by chat commands.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/artwork"
	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/config"
	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/jellyfin"
	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/logging"
	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/matrix"
	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/player"
	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/rtc"
)

// Stamped at build time with -ldflags; see the Dockerfile.
var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config.yaml")
	showVersion := flag.Bool("version", false, "print version and exit")
	check := flag.Bool("check", false, "check ffmpeg, its encoders and the artwork renderer, then exit")
	ffmpegPath := flag.String("ffmpeg", "ffmpeg", "ffmpeg binary to use with -check")
	flag.Parse()

	if *showVersion {
		fmt.Printf("musicbot %s (%s) built %s\n", version, commit, buildTime)
		return
	}

	if *check {
		fmt.Printf("musicbot %s (%s) built %s\n", version, commit, buildTime)
		if err := preflight(*ffmpegPath); err != nil {
			fmt.Fprintln(os.Stderr, "preflight failed:", err)
			os.Exit(1)
		}
		return
	}

	if err := run(*configPath); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// leaveTimeout bounds the shutdown path: leaving a call happens after the
// context that drove the bot has already been cancelled.
const leaveTimeout = 15 * time.Second

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	// Cancelled on SIGINT/SIGTERM so shutdown can retract the call membership.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log, err := logging.New(cfg.LogLevel)
	if err != nil {
		return err
	}
	// The LiveKit SDK and pion log through their own global logger; route it
	// into ours so everything lands in one stream at a sane volume.
	lksdk.SetLogger(logging.LiveKit(log, cfg.LogLevel == "debug"))

	client, err := mautrix.NewClient(cfg.Matrix.Homeserver, id.UserID(cfg.Matrix.UserID), cfg.Matrix.AccessToken)
	if err != nil {
		return fmt.Errorf("create matrix client: %w", err)
	}
	client.Log = log
	log.Info().
		Str("version", version).
		Str("commit", commit).
		Str("built", buildTime).
		Msg("starting musicbot")

	// The device ID must match the one the access token belongs to: it is half
	// of the LiveKit identity the JWT service derives.
	deviceID := cfg.Matrix.DeviceID
	if deviceID == "" {
		whoami, err := client.Whoami(ctx)
		if err != nil {
			return fmt.Errorf("whoami (set matrix.device_id to skip): %w", err)
		}
		deviceID = whoami.DeviceID.String()
	}
	client.DeviceID = id.DeviceID(deviceID)
	client.Log.Info().Str("user_id", cfg.Matrix.UserID).Str("device_id", deviceID).Msg("matrix client ready")

	jf := jellyfin.New(cfg.Jellyfin.Server, cfg.Jellyfin.APIKey, cfg.Jellyfin.UserID)
	if err := jf.Ping(ctx); err != nil {
		return err
	}
	client.Log.Info().Str("server", cfg.Jellyfin.Server).Msg("jellyfin reachable")

	roomID := id.RoomID(cfg.Matrix.RoomID)
	if _, err := client.JoinRoomByID(ctx, roomID); err != nil {
		return fmt.Errorf("join room %s: %w", roomID, err)
	}

	// Join the call. The bot speaks two MatrixRTC dialects: the session-style
	// membership Element Call has always used, and the MSC4143 membership with
	// MSC4354 sticky events that replaces it. They derive different LiveKit
	// identities, so being audible to both audiences means one SFU connection
	// per dialect.
	caps, err := rtc.Probe(ctx, client)
	if err != nil {
		client.Log.Warn().Err(err).Msg("could not read homeserver features; assuming the legacy MatrixRTC stack only")
	}
	useSticky := cfg.RTC.UsesSticky() && caps.SupportsStickyMembership()
	if cfg.RTC.UsesSticky() && !useSticky {
		client.Log.Warn().
			Bool("msc4354", caps.StickyEvents).
			Bool("msc4143", caps.MatrixRTC).
			Msg("homeserver does not support sticky MatrixRTC membership; using the legacy stack only")
	}

	membership := rtc.NewMembership(client, roomID, id.UserID(cfg.Matrix.UserID), deviceID)

	// The service the bot would pick for itself, when there is no call in
	// progress to defer to.
	preferred := cfg.RTC.LiveKitServiceURL
	if preferred == "" {
		preferred, err = rtc.DiscoverService(ctx, cfg.Matrix.Homeserver)
		if err != nil {
			return err
		}
		client.Log.Info().Str("service", preferred).Msg("discovered MatrixRTC service")
	}
	focus := rtc.NewFocus(preferred)

	// Which focus to join on is decided on every entry to the call rather than
	// once at startup. The bot leaves a call that has emptied out, and the
	// person who starts the next one picks the focus that MatrixRTC's "oldest
	// membership wins" rule then puts everybody on — including the bot.
	resolveFocus := func(ctx context.Context) (string, error) {
		transport, err := membership.ActiveTransport(ctx)
		if err != nil {
			return focus.Preferred(), err
		}
		if transport != nil && transport.LiveKitServiceURL != "" {
			return transport.LiveKitServiceURL, nil
		}
		return focus.Preferred(), nil
	}

	var legs []rtc.NamedPublisher
	var members []rtc.CallMember
	var alias string

	if cfg.RTC.UsesLegacy() {
		// dial is the whole join, and it runs on every entry to the call and
		// every reconnection: the LiveKit token is minted from a single-use
		// OpenID token, so getting back in never means reusing a JWT.
		dial := func(ctx context.Context) (*rtc.Publisher, error) {
			sfu, err := rtc.GetSFUConfig(ctx, client, focus.ServiceURL(), roomID, deviceID, "", "", 0)
			if err != nil {
				return nil, fmt.Errorf("get livekit token: %w", err)
			}
			// The LiveKit room is only known once a token has been minted, and
			// the membership published straight after this has to name it.
			focus.SetAlias(sfu.Alias)
			return rtc.Connect(sfu, cfg.RTC.DisplayName, cfg.Audio.Channels())
		}

		// A token minted at startup and then thrown away is worth one round
		// trip: it fails a bad access token or an unreachable focus here,
		// rather than at whatever hour the first listener turns up.
		sfu, err := rtc.GetSFUConfig(ctx, client, focus.ServiceURL(), roomID, deviceID, "", "", 0)
		if err != nil {
			return fmt.Errorf("get livekit token: %w", err)
		}
		client.Log.Info().
			Str("url", sfu.URL).Str("alias", sfu.Alias).Str("identity", sfu.Identity).
			Str("source", string(sfu.Source)).Msg("got livekit credentials")
		focus.SetAlias(sfu.Alias)

		// Watch the published membership, not just the delayed leave: the
		// keeper only heals a leave the homeserver fired, and the membership
		// can go missing without that. It stands down while the bot is out of
		// the call, so it costs nothing then.
		go membership.Watch(ctx)

		legs = append(legs, rtc.NamedPublisher{Name: "legacy", Dial: dial})
		members = append(members, membership)
		alias = sfu.Alias
	}

	if useSticky {
		// Sticky membership is inert without an open slot: clients treat a
		// member of a missing or closed slot as having left.
		if err := rtc.EnsureSlot(ctx, client, roomID, cfg.RTC.SlotID); err != nil {
			return err
		}
		sticky, err := rtc.NewStickyMembership(client, roomID, cfg.RTC.SlotID, cfg.RTC.StickyDuration)
		if err != nil {
			return err
		}
		// The member ID is fixed for the life of this membership and the
		// LiveKit identity is derived from it, so a reconnection lands back on
		// the same identity the published membership points at.
		dial := func(ctx context.Context) (*rtc.Publisher, error) {
			sfu, err := rtc.GetStickyToken(ctx, client, focus.ServiceURL(), roomID, cfg.RTC.SlotID, sticky.MemberID(), deviceID)
			if err != nil {
				return nil, fmt.Errorf("get livekit token for sticky membership: %w", err)
			}
			if sfu == nil {
				return nil, fmt.Errorf("no /get_token endpoint available")
			}
			return rtc.Connect(sfu, cfg.RTC.DisplayName, cfg.Audio.Channels())
		}

		sfu, err := rtc.GetStickyToken(ctx, client, focus.ServiceURL(), roomID, cfg.RTC.SlotID, sticky.MemberID(), deviceID)
		switch {
		case err != nil:
			return fmt.Errorf("get livekit token for sticky membership: %w", err)
		case sfu == nil:
			client.Log.Warn().Msg("no /get_token endpoint available; skipping sticky MatrixRTC membership")
		case alias != "" && sfu.Alias != alias:
			// Two aliases means two different LiveKit rooms, so the second
			// connection would publish where nobody is listening.
			client.Log.Warn().
				Str("legacy_alias", alias).Str("sticky_alias", sfu.Alias).
				Msg("token endpoints disagree on the livekit room; skipping sticky MatrixRTC membership")
		default:
			client.Log.Info().
				Str("url", sfu.URL).Str("alias", sfu.Alias).Str("identity", sfu.Identity).
				Str("source", string(sfu.Source)).Msg("got livekit credentials for sticky membership")

			legs = append(legs, rtc.NamedPublisher{Name: "sticky", Dial: dial})
			members = append(members, rtc.StickyMember(sticky))
		}
	}

	if len(legs) == 0 {
		return fmt.Errorf("no MatrixRTC connection established; check rtc.stack in config.yaml")
	}
	// The publisher outlives any one visit to the call: the player, the
	// artwork and the commands all hold it for the life of the process, while
	// the connections behind it come and go with the audience.
	publisher := rtc.NewIdleMultiPublisher(log, legs...)
	// Closing the group is what stops the reconnect loops and disconnects
	// whatever connection each leg is on now, which after a reconnection is no
	// longer the one it was first given.
	defer publisher.Close()
	call := rtc.NewSession(log, focus, publisher, resolveFocus, members...)
	defer func() {
		leaveCtx, cancel := context.WithTimeout(context.Background(), leaveTimeout)
		defer cancel()
		if err := call.Leave(leaveCtx); err != nil {
			client.Log.Err(err).Msg("failed to leave call cleanly")
		}
	}()
	client.Log.Info().
		Str("bitrate", cfg.Audio.Bitrate).
		Str("vbr", cfg.Audio.VBR).
		Int("channels", cfg.Audio.Channels()).
		Int("fec_packet_loss", cfg.Audio.FECPacketLoss).
		Msg("ready to publish")

	if cfg.RTC.LeavesWhenAlone() {
		// The bot joins when the first listener does. Whether anybody is in the
		// call already is a question for the watcher, which answers it as soon
		// as it has read the room.
		client.Log.Info().Dur("linger", cfg.RTC.Linger).
			Msg("staying out of the call until somebody is in it")
	} else if err := call.Enter(ctx); err != nil {
		return err
	}

	// The album cover is published as a still video track, so the bot shows a
	// picture in the call instead of an empty tile. It is decorative: if any
	// of it fails the bot carries on as an audio-only participant.
	var artPublisher *artwork.Publisher
	if cfg.Audio.ShowArtVideo() {
		if err := publisher.PublishVideo(cfg.RTC.DisplayName); err != nil {
			client.Log.Warn().Err(err).Msg("could not publish album art video track; continuing audio-only")
		} else {
			artPublisher = artwork.NewPublisher(
				artwork.NewRenderer(cfg.Player.FFmpegPath), jf, publisher,
				artwork.IdleText{
					Title: "Nothing playing",
					Hint:  cfg.Matrix.CommandPrefix + "play <song>",
				},
				log)
			defer artPublisher.Close()
			// Put the idle card up straight away, so the tile is never blank.
			artPublisher.Show(nil)
			client.Log.Info().Msg("publishing album art as a video track")
		}
	}

	// The bot and the player reference each other: the player reports track
	// changes to chat, the bot drives the player. Build the player first with a
	// notify hook that resolves once the bot exists.
	var bot *matrix.Bot
	encode := player.EncodeOptions{
		Bitrate:       cfg.Audio.Bitrate,
		VBR:           cfg.Audio.VBR,
		Channels:      cfg.Audio.Channels(),
		FECPacketLoss: cfg.Audio.FECPacketLoss,
	}
	plr := player.New(player.Options{
		Publisher:  publisher,
		FFmpegPath: cfg.Player.FFmpegPath,
		MaxQueue:   cfg.Player.MaxQueue,
		Encode:     encode,
		URLFor:     func(item jellyfin.Item) string { return jf.StreamURL(item.ID) },
		Notify: func(msg string) {
			if bot != nil {
				bot.Notify(msg)
			}
		},
		TrackChanged: func(item *jellyfin.Item) {
			if bot != nil {
				bot.TrackChanged(item)
			}
		},
		// Tell Jellyfin what is playing, so the bot shows up in the dashboard
		// like any other client and listens count towards play counts.
		Reporter:         jellyfin.NewPlaybackReporter(jf, log),
		ProgressInterval: jellyfin.ProgressInterval,
	})
	defer plr.Close()

	bot = matrix.New(cfg, client, jf, plr)
	bot.SetCall(call)
	if artPublisher != nil {
		bot.SetArtPublisher(artPublisher)
	}
	client.Log.Info().Str("room", cfg.Matrix.RoomID).Msg("listening for commands")

	if err := bot.Run(ctx); err != nil && ctx.Err() == nil {
		return fmt.Errorf("sync: %w", err)
	}
	client.Log.Info().Msg("shutting down")
	return nil
}
