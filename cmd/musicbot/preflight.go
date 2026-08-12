package main

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/artwork"
)

// requiredEncoders are the ffmpeg encoders the bot cannot work without: Opus
// carries the audio and x264 renders the album art tile.
var requiredEncoders = []string{"libopus", "libx264"}

// preflight checks that the environment can actually do what the bot needs,
// and reports what it found. It exists because the alternative is discovering a
// missing encoder or font part way through a track, in a log nobody is reading.
func preflight(ffmpegPath string) error {
	var problems []string

	version, err := exec.Command(ffmpegPath, "-hide_banner", "-version").Output()
	if err != nil {
		fmt.Printf("ffmpeg (%s): NOT FOUND: %v\n", ffmpegPath, err)
		return fmt.Errorf("ffmpeg is required")
	}
	fmt.Printf("ffmpeg:   %s\n", firstLine(strings.TrimSpace(string(version))))

	encoders, err := exec.Command(ffmpegPath, "-hide_banner", "-encoders").Output()
	if err != nil {
		return fmt.Errorf("listing ffmpeg encoders: %w", err)
	}
	for _, encoder := range requiredEncoders {
		if strings.Contains(string(encoders), encoder) {
			fmt.Printf("encoder:  %s ok\n", encoder)
		} else {
			fmt.Printf("encoder:  %s MISSING\n", encoder)
			problems = append(problems, "ffmpeg lacks "+encoder)
		}
	}

	// Rendering a real tile exercises the whole artwork path: the font lookup,
	// ffmpeg's drawtext filter, and the H.264 access unit check.
	renderer := artwork.NewRenderer(ffmpegPath)
	if font := renderer.FontFile(); font != "" {
		fmt.Printf("font:     %s\n", font)
	} else {
		fmt.Println("font:     none found; placeholder tiles will have no text")
	}
	frame, err := renderer.Placeholder("Preflight", "musicbot")
	if err != nil {
		fmt.Printf("artwork:  FAILED: %v\n", err)
		problems = append(problems, "cannot render album art")
	} else {
		fmt.Printf("artwork:  rendered a %d byte keyframe\n", len(frame))
	}

	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	fmt.Println("preflight ok")
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
