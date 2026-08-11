package player

import (
	"os/exec"
	"strings"
	"testing"
)

// argValue returns the value following flag in args.
func argValue(args []string, flag string) (string, bool) {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func hasFlag(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
	}
	return false
}

func TestFFmpegArgsCarryEncodeOptions(t *testing.T) {
	opts := EncodeOptions{Bitrate: "160k", VBR: "on", Channels: 1, FECPacketLoss: 7}
	args := opts.ffmpegArgs("http://example.org/track")

	for flag, want := range map[string]string{
		"-b:a":            "160k",
		"-vbr":            "on",
		"-ac":             "1",
		"-packet_loss":    "7",
		"-ar":             "48000",
		"-frame_duration": "20",
		"-application":    "audio",
		// One Opus packet per Ogg page keeps page and RTP sample one-to-one.
		"-page_duration": "20000",
	} {
		got, ok := argValue(args, flag)
		if !ok {
			t.Errorf("%s missing from ffmpeg args", flag)
			continue
		}
		if got != want {
			t.Errorf("%s = %q; want %q", flag, got, want)
		}
	}
}

func TestFFmpegArgsOmitFECWhenZero(t *testing.T) {
	args := EncodeOptions{Bitrate: "128k", VBR: "constrained", Channels: 2}.ffmpegArgs("http://example.org/t")
	if hasFlag(args, "-packet_loss") {
		t.Error("-packet_loss present with FECPacketLoss 0; FEC should be off by default")
	}
}

// The reconnect options are HTTP-only: ffmpeg exits with "Option reconnect not
// found" if they are passed for a local file.
func TestFFmpegArgsReconnectOnlyForHTTP(t *testing.T) {
	opts := EncodeOptions{Bitrate: "128k", VBR: "constrained", Channels: 2}

	if !hasFlag(opts.ffmpegArgs("https://jellyfin.example/Audio/1/stream"), "-reconnect") {
		t.Error("-reconnect missing for an http url")
	}
	if hasFlag(opts.ffmpegArgs("/tmp/track.flac"), "-reconnect") {
		t.Error("-reconnect present for a local file; ffmpeg rejects it there")
	}
}

// TestEncodeOptionsProduceRequestedChannels runs ffmpeg for real and checks the
// channel count of what comes out, since stereo has to survive the encoder for
// any of the SDP negotiation to matter.
func TestEncodeOptionsProduceRequestedChannels(t *testing.T) {
	requireFFmpeg(t)
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	dir := t.TempDir()
	tone := makeTone(t, dir, "tone.wav", 1, 440)

	for _, tc := range []struct{ channels int }{{1}, {2}} {
		opts := EncodeOptions{Bitrate: "128k", VBR: "constrained", Channels: tc.channels}
		out := dir + "/out.ogg"
		args := append(opts.ffmpegArgs(tone)[:0:0], opts.ffmpegArgs(tone)...)
		args[len(args)-1] = out // write to a file instead of stdout
		args = append([]string{"-y"}, args...)
		if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg with %d channels: %v: %s", tc.channels, err, out)
		}

		probe, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0",
			"-show_entries", "stream=channels", "-of", "csv=p=0", out).Output()
		if err != nil {
			t.Fatalf("ffprobe: %v", err)
		}
		if got := strings.TrimSpace(string(probe)); got != map[int]string{1: "1", 2: "2"}[tc.channels] {
			t.Errorf("encoded channels = %s; want %d", got, tc.channels)
		}
	}
}
