package artwork

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
}

// makeCover produces a JPEG standing in for album art.
func makeCover(t *testing.T) []byte {
	t.Helper()
	path := t.TempDir() + "/cover.jpg"
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=600x600:duration=1", "-frames:v", "1", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make cover: %v: %s", err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// assertSelfContainedKeyframe checks the Annex-B stream has SPS, PPS and IDR.
func assertSelfContainedKeyframe(t *testing.T, frame []byte) {
	t.Helper()
	var seen []byte
	for _, nal := range splitAnnexB(frame) {
		seen = append(seen, nal[0]&0x1f)
	}
	has := func(want byte) bool {
		for _, got := range seen {
			if got == want {
				return true
			}
		}
		return false
	}
	if !has(nalSPS) || !has(nalPPS) {
		t.Errorf("frame lacks SPS/PPS; NAL types present: %v", seen)
	}
	if !has(nalIDR) {
		t.Errorf("frame lacks an IDR picture; NAL types present: %v", seen)
	}
}

func TestFromImageProducesKeyframe(t *testing.T) {
	requireFFmpeg(t)
	frame, err := NewRenderer("ffmpeg").FromImage(makeCover(t))
	if err != nil {
		t.Fatalf("FromImage() error: %v", err)
	}
	if len(frame) == 0 {
		t.Fatal("FromImage() returned an empty frame")
	}
	// The access unit must carry its own parameter sets and an IDR picture, or
	// a receiver joining later has nothing to decode against.
	assertSelfContainedKeyframe(t, frame)
	// Sanity on size: far too large means the encoder ignored the bitrate.
	if len(frame) > 60_000 {
		t.Errorf("keyframe is %d bytes; expected well under 60KB", len(frame))
	}
	t.Logf("keyframe %d bytes", len(frame))
}

func TestPlaceholderProducesKeyframe(t *testing.T) {
	requireFFmpeg(t)
	frame, err := NewRenderer("ffmpeg").Placeholder("Ocarina of Time", "Rozen")
	if err != nil {
		t.Fatalf("Placeholder() error: %v", err)
	}
	if len(frame) == 0 {
		t.Fatal("Placeholder() returned an empty frame")
	}
	assertSelfContainedKeyframe(t, frame)
}

// Track names routinely contain quotes, colons and commas, all of which are
// filtergraph syntax. They must not break the render or inject filters.
func TestPlaceholderHandlesFiltergraphMetacharacters(t *testing.T) {
	requireFFmpeg(t)
	renderer := NewRenderer("ffmpeg")
	for _, name := range []string{
		`"It's a Me, Mario!"`,
		`A:B [remix]`,
		`100% Orange Juice`,
		`back\slash`,
	} {
		if _, err := renderer.Placeholder(name, "Some, Artist: Two"); err != nil {
			t.Errorf("Placeholder(%q) error: %v", name, err)
		}
	}
}

func TestFromImageRejectsGarbage(t *testing.T) {
	requireFFmpeg(t)
	_, err := NewRenderer("ffmpeg").FromImage([]byte("not an image"))
	if err == nil {
		t.Fatal("FromImage() accepted non-image data; want an error")
	}
	if !strings.Contains(err.Error(), "artwork") {
		t.Errorf("error %q should mention artwork rendering", err)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate() = %q; want it unchanged", got)
	}
	if got := []rune(truncate(strings.Repeat("x", 40), 10)); len(got) != 10 {
		t.Errorf("truncate() produced %d runes; want 10", len(got))
	}
}
