// Package config loads and validates the bot's config.yaml.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root of config.yaml.
type Config struct {
	Matrix   Matrix   `yaml:"matrix"`
	RTC      RTC      `yaml:"rtc"`
	Jellyfin Jellyfin `yaml:"jellyfin"`
	Player   Player   `yaml:"player"`
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
}

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
	if c.Matrix.CommandPrefix == "" {
		c.Matrix.CommandPrefix = "!"
	}
	if c.RTC.DisplayName == "" {
		c.RTC.DisplayName = "Jukebox"
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
	return nil
}

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
