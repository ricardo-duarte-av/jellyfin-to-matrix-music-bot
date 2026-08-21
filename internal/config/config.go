// Package config loads and validates the bot's config.yaml.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root of config.yaml.
type Config struct {
	// LogLevel is one of debug, info, warn, error. debug also un-mutes the
	// LiveKit and pion logging, which is very verbose.
	LogLevel string   `yaml:"log_level"`
	Matrix   Matrix   `yaml:"matrix"`
	RTC      RTC      `yaml:"rtc"`
	Jellyfin Jellyfin `yaml:"jellyfin"`
	Player   Player   `yaml:"player"`
	Audio    Audio    `yaml:"audio"`
}

// Audio controls how ffmpeg encodes Opus for the call.
type Audio struct {
	// Bitrate is the target, in ffmpeg syntax ("128k").
	Bitrate string `yaml:"bitrate"`
	// VBR is "constrained" (hold close to the target), "on" (free-running
	// variable rate, which overshoots the target by roughly 15%), or "off"
	// (constant rate).
	VBR string `yaml:"vbr"`
	// Stereo publishes two channels. Mono roughly halves the bitrate needed
	// for the same perceived quality. It is a pointer so that an explicit
	// "stereo: false" is distinguishable from the key being absent.
	Stereo *bool `yaml:"stereo"`
	// FECPacketLoss is the packet loss percentage to encode inband FEC for.
	// 0 disables FEC; 5-10 is a reasonable range for a lossy network. Higher
	// values spend more of the bitrate on redundancy.
	FECPacketLoss int `yaml:"fec_packet_loss"`
	// AlbumArtVideo publishes the current cover as a still video track, so the
	// bot shows a picture in the call rather than an empty tile. Costs about
	// 12KB per track change plus an occasional keyframe refresh.
	AlbumArtVideo *bool `yaml:"album_art_video"`
	// AlbumArtChat sends now-playing announcements as the cover image with the
	// track details as its caption.
	AlbumArtChat *bool `yaml:"album_art_chat"`
}

// ShowArtVideo reports whether to publish the album-art video track.
func (a Audio) ShowArtVideo() bool { return a.AlbumArtVideo == nil || *a.AlbumArtVideo }

// ShowArtChat reports whether now-playing messages carry the cover image.
func (a Audio) ShowArtChat() bool { return a.AlbumArtChat == nil || *a.AlbumArtChat }

// IsStereo reports whether two channels are published.
func (a Audio) IsStereo() bool { return a.Stereo == nil || *a.Stereo }

// Channels is the channel count implied by the stereo setting.
func (a Audio) Channels() int {
	if a.IsStereo() {
		return 2
	}
	return 1
}

// Matrix holds homeserver connection details and the room the bot serves.
type Matrix struct {
	Homeserver    string   `yaml:"homeserver"`
	UserID        string   `yaml:"user_id"`
	AccessToken   string   `yaml:"access_token"`
	DeviceID      string   `yaml:"device_id"`
	RoomID        string   `yaml:"room_id"`
	Admins        []string `yaml:"admins"`
	CommandPrefix string   `yaml:"command_prefix"`
}

// RTC holds MatrixRTC / LiveKit settings.
type RTC struct {
	// LiveKitServiceURL overrides .well-known discovery of the MatrixRTC
	// authorization service when set.
	LiveKitServiceURL string `yaml:"livekit_service_url"`
	DisplayName       string `yaml:"display_name"`
	// Stack selects which MatrixRTC dialect the bot speaks: "legacy" for the
	// session-style membership Element Call has always used, "sticky" for the
	// MSC4143/MSC4354 membership replacing it, or "both".
	//
	// "both" costs a second SFU connection, and so doubles the bot's upstream
	// bandwidth: the two dialects derive different LiveKit identities, and one
	// connection can only be one of them.
	Stack string `yaml:"stack"`
	// StickyDuration is how long a published sticky membership stays valid
	// without a refresh. Capped at an hour by MSC4354.
	StickyDuration time.Duration `yaml:"sticky_duration"`
	// SlotID is the MatrixRTC slot to join. The default is the room-wide call
	// every calling client uses.
	SlotID string `yaml:"slot_id"`
}

// MatrixRTC stack selections.
const (
	StackLegacy = "legacy"
	StackSticky = "sticky"
	StackBoth   = "both"
)

// UsesLegacy reports whether the session-style membership should be published.
func (r RTC) UsesLegacy() bool { return r.Stack == StackLegacy || r.Stack == StackBoth }

// UsesSticky reports whether the MSC4143 membership should be published.
func (r RTC) UsesSticky() bool { return r.Stack == StackSticky || r.Stack == StackBoth }

// Jellyfin holds the media server connection details.
type Jellyfin struct {
	Server string `yaml:"server"`
	APIKey string `yaml:"api_key"`
	// UserID scopes library views and playlists; Jellyfin's item APIs need it.
	UserID string `yaml:"user_id"`
}

// Player holds playback and search tuning.
type Player struct {
	SearchLimit int           `yaml:"search_limit"`
	ResultTTL   time.Duration `yaml:"result_ttl"`
	MaxQueue    int           `yaml:"max_queue"`
	FFmpegPath  string        `yaml:"ffmpeg_path"`
}

// Load reads, defaults and validates the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.Matrix.CommandPrefix == "" {
		c.Matrix.CommandPrefix = "!"
	}
	if c.RTC.DisplayName == "" {
		c.RTC.DisplayName = "Jukebox"
	}
	if c.RTC.Stack == "" {
		c.RTC.Stack = StackBoth
	}
	if c.RTC.StickyDuration <= 0 {
		c.RTC.StickyDuration = 10 * time.Minute
	}
	if c.RTC.SlotID == "" {
		c.RTC.SlotID = "m.call#ROOM"
	}
	if c.Player.SearchLimit <= 0 {
		c.Player.SearchLimit = 10
	}
	if c.Player.ResultTTL <= 0 {
		c.Player.ResultTTL = 10 * time.Minute
	}
	if c.Player.MaxQueue <= 0 {
		c.Player.MaxQueue = 200
	}
	if c.Player.FFmpegPath == "" {
		c.Player.FFmpegPath = "ffmpeg"
	}
	if c.Audio.Bitrate == "" {
		c.Audio.Bitrate = "128k"
	}
	if c.Audio.VBR == "" {
		// Constrained by default so the configured bitrate is the bitrate
		// actually sent; free-running VBR overshoots it noticeably.
		c.Audio.VBR = "constrained"
	}
	c.Matrix.Homeserver = strings.TrimSuffix(c.Matrix.Homeserver, "/")
	c.Jellyfin.Server = strings.TrimSuffix(c.Jellyfin.Server, "/")
	c.RTC.LiveKitServiceURL = strings.TrimSuffix(c.RTC.LiveKitServiceURL, "/")
}

func (c *Config) validate() error {
	var missing []string
	require := func(key, value string) {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, key)
		}
	}
	require("matrix.homeserver", c.Matrix.Homeserver)
	require("matrix.user_id", c.Matrix.UserID)
	require("matrix.access_token", c.Matrix.AccessToken)
	require("matrix.room_id", c.Matrix.RoomID)
	require("jellyfin.server", c.Jellyfin.Server)
	require("jellyfin.api_key", c.Jellyfin.APIKey)
	require("jellyfin.user_id", c.Jellyfin.UserID)
	if len(missing) > 0 {
		return fmt.Errorf("config is missing required keys: %s", strings.Join(missing, ", "))
	}

	if !strings.HasPrefix(c.Matrix.UserID, "@") {
		return fmt.Errorf("matrix.user_id must start with '@', got %q", c.Matrix.UserID)
	}
	if !strings.HasPrefix(c.Matrix.RoomID, "!") {
		return fmt.Errorf("matrix.room_id must be an internal room ID starting with '!', got %q", c.Matrix.RoomID)
	}
	if !strings.HasPrefix(c.Matrix.Homeserver, "http://") && !strings.HasPrefix(c.Matrix.Homeserver, "https://") {
		return fmt.Errorf("matrix.homeserver must be a http(s) URL, got %q", c.Matrix.Homeserver)
	}
	if !strings.HasPrefix(c.Jellyfin.Server, "http://") && !strings.HasPrefix(c.Jellyfin.Server, "https://") {
		return fmt.Errorf("jellyfin.server must be a http(s) URL, got %q", c.Jellyfin.Server)
	}
	for i, admin := range c.Matrix.Admins {
		if !strings.HasPrefix(admin, "@") {
			return fmt.Errorf("matrix.admins[%d] must start with '@', got %q", i, admin)
		}
	}

	switch c.RTC.Stack {
	case StackLegacy, StackSticky, StackBoth:
	default:
		return fmt.Errorf("rtc.stack must be legacy, sticky or both, got %q", c.RTC.Stack)
	}
	if c.RTC.StickyDuration > time.Hour {
		return fmt.Errorf("rtc.sticky_duration must not exceed 1h, got %s", c.RTC.StickyDuration)
	}
	switch c.Audio.VBR {
	case "on", "constrained", "off":
	default:
		return fmt.Errorf("audio.vbr must be on, constrained or off, got %q", c.Audio.VBR)
	}
	if c.Audio.FECPacketLoss < 0 || c.Audio.FECPacketLoss > 100 {
		return fmt.Errorf("audio.fec_packet_loss must be a percentage between 0 and 100, got %d", c.Audio.FECPacketLoss)
	}
	if !bitratePattern.MatchString(c.Audio.Bitrate) {
		return fmt.Errorf("audio.bitrate must look like 128k or 96000, got %q", c.Audio.Bitrate)
	}
	return nil
}

// bitratePattern accepts ffmpeg's bitrate syntax: a number, optionally with a
// k or M suffix.
var bitratePattern = regexp.MustCompile(`^[0-9]+[kKmM]?$`)

// IsAdmin reports whether userID may run control commands. An empty admin list
// means everyone is allowed.
func (c *Config) IsAdmin(userID string) bool {
	if len(c.Matrix.Admins) == 0 {
		return true
	}
	for _, admin := range c.Matrix.Admins {
		if admin == userID {
			return true
		}
	}
	return false
}
