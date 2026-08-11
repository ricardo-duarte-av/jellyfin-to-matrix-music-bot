package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validConfig = `
matrix:
  homeserver: https://matrix.example.org
  user_id: "@bot:example.org"
  access_token: "token"
  device_id: "DEV"
  room_id: "!room:example.org"
  admins: ["@me:example.org"]
jellyfin:
  server: http://jellyfin.local:8096
  api_key: "key"
  user_id: "uid"
`

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := Load(write(t, validConfig))
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Matrix.CommandPrefix != "!" {
		t.Errorf("CommandPrefix = %q; want !", cfg.Matrix.CommandPrefix)
	}
	if cfg.Player.SearchLimit != 10 {
		t.Errorf("SearchLimit = %d; want 10", cfg.Player.SearchLimit)
	}
	if cfg.Player.ResultTTL != 10*time.Minute {
		t.Errorf("ResultTTL = %v; want 10m", cfg.Player.ResultTTL)
	}
	if cfg.Player.MaxQueue != 200 {
		t.Errorf("MaxQueue = %d; want 200", cfg.Player.MaxQueue)
	}
	if cfg.Player.FFmpegPath != "ffmpeg" {
		t.Errorf("FFmpegPath = %q; want ffmpeg", cfg.Player.FFmpegPath)
	}
	if cfg.RTC.DisplayName != "Jukebox" {
		t.Errorf("DisplayName = %q; want Jukebox", cfg.RTC.DisplayName)
	}
}

func TestLoadTrimsTrailingSlashes(t *testing.T) {
	body := strings.Replace(validConfig, "http://jellyfin.local:8096", "http://jellyfin.local:8096/", 1)
	cfg, err := Load(write(t, body))
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Jellyfin.Server != "http://jellyfin.local:8096" {
		t.Errorf("Server = %q; want the trailing slash trimmed", cfg.Jellyfin.Server)
	}
}

func TestLoadRejectsMissingKeys(t *testing.T) {
	body := strings.Replace(validConfig, `  api_key: "key"`, "", 1)
	_, err := Load(write(t, body))
	if err == nil {
		t.Fatal("Load() succeeded with no jellyfin.api_key; want an error")
	}
	if !strings.Contains(err.Error(), "jellyfin.api_key") {
		t.Errorf("error %q does not name the missing key", err)
	}
}

func TestLoadRejectsMalformedIDs(t *testing.T) {
	tests := map[string]struct{ from, to string }{
		"user id without sigil": {`user_id: "@bot:example.org"`, `user_id: "bot:example.org"`},
		"room alias not id":     {`room_id: "!room:example.org"`, `room_id: "#room:example.org"`},
		"homeserver not a url":  {"homeserver: https://matrix.example.org", "homeserver: matrix.example.org"},
		"admin without sigil":   {`admins: ["@me:example.org"]`, `admins: ["me:example.org"]`},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			body := strings.Replace(validConfig, tc.from, tc.to, 1)
			if _, err := Load(write(t, body)); err == nil {
				t.Errorf("Load() succeeded with %s; want an error", name)
			}
		})
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	body := validConfig + "\nnonsense: true\n"
	if _, err := Load(write(t, body)); err == nil {
		t.Error("Load() accepted an unknown top-level key; want an error so typos are caught")
	}
}

func TestAudioDefaults(t *testing.T) {
	cfg, err := Load(write(t, validConfig))
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Audio.Bitrate != "128k" {
		t.Errorf("Bitrate = %q; want 128k", cfg.Audio.Bitrate)
	}
	// Constrained by default so the configured number is the number sent.
	if cfg.Audio.VBR != "constrained" {
		t.Errorf("VBR = %q; want constrained", cfg.Audio.VBR)
	}
	if !cfg.Audio.IsStereo() || cfg.Audio.Channels() != 2 {
		t.Errorf("stereo default = %v (%d channels); want stereo", cfg.Audio.IsStereo(), cfg.Audio.Channels())
	}
	if cfg.Audio.FECPacketLoss != 0 {
		t.Errorf("FECPacketLoss = %d; want 0", cfg.Audio.FECPacketLoss)
	}
}

// An explicit "stereo: false" must be honoured, which is why the field is a
// pointer: a plain bool cannot tell it apart from the key being absent.
func TestAudioStereoCanBeDisabled(t *testing.T) {
	cfg, err := Load(write(t, validConfig+"\naudio:\n  stereo: false\n"))
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Audio.IsStereo() {
		t.Error("IsStereo() = true after an explicit stereo: false")
	}
	if got := cfg.Audio.Channels(); got != 1 {
		t.Errorf("Channels() = %d; want 1", got)
	}
}

func TestAudioValidation(t *testing.T) {
	tests := map[string]string{
		"bad vbr mode":     "\naudio:\n  vbr: sometimes\n",
		"bad bitrate":      "\naudio:\n  bitrate: loud\n",
		"fec out of range": "\naudio:\n  fec_packet_loss: 150\n",
		"negative fec":     "\naudio:\n  fec_packet_loss: -1\n",
	}
	for name, extra := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(write(t, validConfig+extra)); err == nil {
				t.Errorf("Load() accepted %s; want an error", name)
			}
		})
	}

	for _, ok := range []string{"128k", "96000", "1M"} {
		if _, err := Load(write(t, validConfig+"\naudio:\n  bitrate: "+ok+"\n")); err != nil {
			t.Errorf("Load() rejected valid bitrate %q: %v", ok, err)
		}
	}
}

func TestIsAdmin(t *testing.T) {
	cfg := &Config{Matrix: Matrix{Admins: []string{"@me:example.org"}}}
	if !cfg.IsAdmin("@me:example.org") {
		t.Error("listed admin was rejected")
	}
	if cfg.IsAdmin("@someone:example.org") {
		t.Error("non-admin was accepted")
	}

	open := &Config{}
	if !open.IsAdmin("@anyone:example.org") {
		t.Error("with an empty admin list everyone should be allowed")
	}
}
