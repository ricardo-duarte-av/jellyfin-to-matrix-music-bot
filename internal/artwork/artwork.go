// Package artwork turns album covers into the single H.264 keyframe the bot
// publishes as its video tile.
package artwork

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Size is the square the artwork is fitted into. Covers are square, and a
// square tile avoids letterboxing in the call layout. 720 is chosen so the
// tile still looks sharp when a client gives the bot a large tile or
// full-screens it.
const Size = 720

// quality is the x264 constant-rate factor. The bot sends one frame per track
// rather than a stream, so rate control by bitrate is the wrong tool: a
// bitrate cap quantises the single frame heavily and the cover arrives blurry.
// CRF 20 keeps it visibly clean at roughly 75KB, which is nothing spread over
// a track.
const quality = "20"

// Renderer encodes images to H.264 keyframes using ffmpeg.
type Renderer struct {
	ffmpegPath string
	fontFile   string
}

// NewRenderer builds a renderer. It looks for a font for the placeholder tile;
// if none is found, placeholders fall back to a plain coloured square.
func NewRenderer(ffmpegPath string) *Renderer {
	return &Renderer{ffmpegPath: ffmpegPath, fontFile: findFont()}
}

// fontCandidates are the usual locations of a DejaVu/Liberation sans font.
var fontCandidates = []string{
	"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
	"/usr/share/fonts/dejavu/DejaVuSans.ttf",
	"/usr/share/fonts/truetype/liberation/LiberationSans-Regular.ttf",
	"/usr/share/fonts/TTF/DejaVuSans.ttf",
	"/System/Library/Fonts/Helvetica.ttc",
}

func findFont() string {
	for _, path := range fontCandidates {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	// Last resort: any ttf under the system font directory.
	matches, _ := filepath.Glob("/usr/share/fonts/**/*.ttf")
	if len(matches) > 0 {
		return matches[0]
	}
	return ""
}

// FromImage encodes cover art into an H.264 keyframe.
func (r *Renderer) FromImage(image []byte) ([]byte, error) {
	filter := fmt.Sprintf(
		"scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=black,format=yuv420p",
		Size, Size, Size, Size)
	return r.encode([]string{"-f", "image2pipe", "-i", "-"}, filter, image)
}

// Placeholder renders a tile carrying the track and artist names, for music
// with no cover art.
func (r *Renderer) Placeholder(title, subtitle string) ([]byte, error) {
	source := []string{"-f", "lavfi", "-i", fmt.Sprintf("color=c=0x1c1c22:s=%dx%d", Size, Size)}
	if r.fontFile == "" {
		return r.encode(source, "format=yuv420p", nil)
	}

	// Track titles routinely contain quotes, colons and commas, which are all
	// filtergraph syntax. Passing the text through a file rather than inline
	// sidesteps the escaping entirely, so no title can break the render or
	// inject extra filters.
	dir, err := os.MkdirTemp("", "musicbot-art-")
	if err != nil {
		return nil, fmt.Errorf("render artwork: %w", err)
	}
	defer os.RemoveAll(dir)

	filter, err := r.drawText(dir, title, subtitle)
	if err != nil {
		return nil, err
	}
	return r.encode(source, filter+",format=yuv420p", nil)
}

// drawText builds the drawtext filters, writing each string to its own file.
func (r *Renderer) drawText(dir, title, subtitle string) (string, error) {
	var filters []string
	add := func(name, text string, size int, y, colour string) error {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(truncate(text, 28)), 0o600); err != nil {
			return fmt.Errorf("render artwork: %w", err)
		}
		filters = append(filters, fmt.Sprintf(
			"drawtext=fontfile=%s:textfile=%s:fontcolor=%s:fontsize=%d:x=(w-text_w)/2:y=%s",
			ffmpegEscape(r.fontFile), ffmpegEscape(path), colour, size, y))
		return nil
	}

	if err := add("title.txt", title, 34, "h/2-40", "white"); err != nil {
		return "", err
	}
	if subtitle != "" {
		if err := add("subtitle.txt", subtitle, 24, "h/2+16", "0xb0b0b8"); err != nil {
			return "", err
		}
	}
	return strings.Join(filters, ","), nil
}

// encode runs ffmpeg to produce one H.264 keyframe and returns the Annex-B
// access unit: SPS, PPS and the IDR picture, which is everything a decoder
// needs to render the image from a cold start.
//
// H.264 rather than VP8 because it is the codec every WebRTC stack offers;
// some LiveKit deployments negotiate H.264/VP9 only, and a VP8 track is then
// rejected outright with "codec is not supported by remote".
func (r *Renderer) encode(source []string, filter string, stdin []byte) ([]byte, error) {
	args := append([]string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y"}, source...)
	args = append(args,
		"-frames:v", "1",
		"-vf", filter,
		"-c:v", "libx264",
		// Constrained baseline is the profile WebRTC clients agree on.
		"-profile:v", "baseline",
		"-level", "3.1",
		"-pix_fmt", "yuv420p",
		// Every frame a keyframe: the bot only ever sends standalone stills.
		// sei=0 drops x264's version banner, ~600 bytes of nothing.
		"-x264-params", "keyint=1:scenecut=0:sei=0",
		"-crf", quality,
		"-f", "h264",
		"-",
	)

	cmd := exec.Command(r.ffmpegPath, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return nil, fmt.Errorf("render artwork: %s", firstLine(msg))
		}
		return nil, fmt.Errorf("render artwork: %w", err)
	}

	frame := stdout.Bytes()
	if err := checkAccessUnit(frame); err != nil {
		return nil, err
	}
	return frame, nil
}

// H.264 NAL unit types the still must contain.
const (
	nalIDR = 5
	nalSPS = 7
	nalPPS = 8
)

// checkAccessUnit verifies ffmpeg produced a self-contained keyframe. Without
// the parameter sets a receiver cannot decode the picture at all, and the tile
// would stay blank with nothing in the logs to explain it.
func checkAccessUnit(frame []byte) error {
	if len(frame) == 0 {
		return fmt.Errorf("render artwork: ffmpeg produced no video frame")
	}
	var seen [32]bool
	for _, nal := range splitAnnexB(frame) {
		seen[nal[0]&0x1f] = true
	}
	switch {
	case !seen[nalSPS], !seen[nalPPS]:
		return fmt.Errorf("render artwork: frame is missing SPS/PPS parameter sets")
	case !seen[nalIDR]:
		return fmt.Errorf("render artwork: frame contains no keyframe picture")
	}
	return nil
}

// splitAnnexB returns the NAL units of an Annex-B byte stream.
func splitAnnexB(data []byte) [][]byte {
	var nals [][]byte
	start := -1
	for i := 0; i+2 < len(data); {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			if start >= 0 && i > start {
				nals = append(nals, data[start:i])
			}
			i += 3
			start = i
			continue
		}
		i++
	}
	if start >= 0 && start < len(data) {
		nals = append(nals, data[start:])
	}
	// Trim the trailing zero byte of a four-byte start code.
	out := make([][]byte, 0, len(nals))
	for _, nal := range nals {
		for len(nal) > 0 && nal[len(nal)-1] == 0 {
			nal = nal[:len(nal)-1]
		}
		if len(nal) > 0 {
			out = append(out, nal)
		}
	}
	return out
}

// ffmpegEscape quotes the characters that would otherwise terminate or split a
// filtergraph argument.
func ffmpegEscape(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		`'`, `\'`,
		`:`, `\:`,
		`%`, `\%`,
		`,`, `\,`,
		`[`, `\[`,
		`]`, `\]`,
		"\n", " ",
	)
	return replacer.Replace(s)
}

func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
