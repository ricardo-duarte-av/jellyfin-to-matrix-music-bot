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

	// Join the call: work out which SFU to use, get a token for it, publish our
	// membership, then connect the media.
	membership := rtc.NewMembership(client, roomID, id.UserID(cfg.Matrix.UserID), deviceID)

	serviceURL := cfg.RTC.LiveKitServiceURL
	if transport, err := membership.ActiveTransport(ctx); err != nil {
		client.Log.Warn().Err(err).Msg("could not inspect existing call memberships")
	} else if transport != nil {
		// Someone is already in the call; MatrixRTC clients all defer to the
		// transport the oldest membership proposed.
		serviceURL = transport.LiveKitServiceURL
		client.Log.Info().Str("service", serviceURL).Msg("joining call on the existing focus")
	}
	if serviceURL == "" {
		serviceURL, err = rtc.DiscoverService(ctx, cfg.Matrix.Homeserver)
		if err != nil {
			return err
		}
		client.Log.Info().Str("service", serviceURL).Msg("discovered MatrixRTC service")
	}

	sfu, err := rtc.GetSFUConfig(ctx, client, serviceURL, roomID, deviceID, "", "", 0)
	if err != nil {
		return fmt.Errorf("get livekit token: %w", err)
	}
	client.Log.Info().Str("url", sfu.URL).Str("alias", sfu.Alias).Str("identity", sfu.Identity).Msg("got livekit credentials")

	if err := membership.Join(ctx, rtc.Transport{
		Type:              rtc.TransportTypeLiveKit,
		LiveKitServiceURL: serviceURL,
		LiveKitAlias:      sfu.Alias,
	}); err != nil {
		return err
	}
	defer func() {
		leaveCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := membership.Leave(leaveCtx); err != nil {
			client.Log.Err(err).Msg("failed to leave call cleanly")
		}
	}()

	publisher, err := rtc.Connect(sfu, cfg.RTC.DisplayName, cfg.Audio.Channels())
	if err != nil {
		return err
	}
	defer publisher.Close()
	client.Log.Info().
		Str("identity", publisher.Identity()).
		Str("bitrate", cfg.Audio.Bitrate).
		Str("vbr", cfg.Audio.VBR).
		Int("channels", cfg.Audio.Channels()).
		Int("fec_packet_loss", cfg.Audio.FECPacketLoss).
		Msg("connected to livekit and publishing")

	// The album cover is published as a still video track, so the bot shows a
	// picture in the call instead of an empty tile. It is decorative: if any
	// of it fails the bot carries on as an audio-only participant.
	var artPublisher *artwork.Publisher
	if cfg.Audio.ShowArtVideo() {
		if err := publisher.PublishVideo(cfg.RTC.DisplayName); err != nil {
			client.Log.Warn().Err(err).Msg("could not publish album art video track; continuing audio-only")
		} else {
			artPublisher = artwork.NewPublisher(artwork.NewRenderer(cfg.Player.FFmpegPath), jf, publisher, log)
			defer artPublisher.Close()
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
	})
	defer plr.Close()

	bot = matrix.New(cfg, client, jf, plr)
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
