package player

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4/pkg/media/oggreader"

	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/rtc"
)

// frameBuffer is how many encoded frames may sit between ffmpeg and the
// publisher. 250 frames is 5 seconds: enough to ride out network hiccups on the
// Jellyfin stream, small enough that skipping a track feels immediate.
const frameBuffer = 250

// Frame is one encoded Opus packet and how much audio it represents.
type Frame struct {
	Data     []byte
	Duration time.Duration
}

// stream is a running ffmpeg process transcoding one track to Ogg/Opus, with a
// goroutine demuxing pages into frames.
type stream struct {
	cmd    *exec.Cmd
	frames chan Frame
	cancel context.CancelFunc

	// closeCh is closed by Close to unblock the demuxer if it is parked on a
	// full frame buffer; drained is closed by the demuxer as it exits.
	closeCh   chan struct{}
	drained   chan struct{}
	closeOnce sync.Once
	waitOnce  sync.Once

	mu      sync.Mutex
	err     error
	waitErr error
	stderr  *strings.Builder
}

// EncodeOptions controls the Opus encode. Because the bot publishes a fixed
// rate and ignores congestion feedback, whatever is set here is what goes on
// the wire for every listener.
type EncodeOptions struct {
	// Bitrate is the target in ffmpeg syntax, e.g. "128k".
	Bitrate string
	// VBR is "on", "constrained" or "off".
	VBR string
	// Channels is 1 or 2.
	Channels int
	// FECPacketLoss, when above zero, turns on inband FEC tuned for that
	// packet loss percentage.
	FECPacketLoss int
}

// ffmpegArgs builds the encoder command line for url.
//
// ffmpeg is asked for one Opus packet per Ogg page (-page_duration 20000), so
// every page maps to exactly one RTP sample and no repacketization is needed.
func (o EncodeOptions) ffmpegArgs(url string) []string {
	args := []string{
		"-nostdin",
		"-hide_banner",
		"-loglevel", "error",
	}
	// The reconnect options belong to ffmpeg's HTTP protocol handler; passing
	// them for a local file makes ffmpeg exit with "Option reconnect not found".
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		args = append(args,
			"-reconnect", "1",
			"-reconnect_streamed", "1",
			"-reconnect_delay_max", "5",
		)
	}
	args = append(args,
		"-i", url,
		"-vn",
		"-map", "a:0",
		"-c:a", "libopus",
		"-b:a", o.Bitrate,
		"-vbr", o.VBR,
		"-ar", fmt.Sprint(rtc.SampleRate),
		"-ac", fmt.Sprint(o.Channels),
		"-frame_duration", "20",
		// "audio" rather than "voip": no speech-tuned processing.
		"-application", "audio",
	)
	if o.FECPacketLoss > 0 {
		args = append(args, "-packet_loss", fmt.Sprint(o.FECPacketLoss))
	}
	return append(args,
		"-f", "ogg",
		"-page_duration", "20000",
		"-",
	)
}

// openStream starts ffmpeg on url and begins producing Opus frames.
func openStream(ffmpegPath, url string, opts EncodeOptions) (*stream, error) {
	args := opts.ffmpegArgs(url)

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}

	s := &stream{
		cmd:     cmd,
		frames:  make(chan Frame, frameBuffer),
		cancel:  cancel,
		stderr:  &stderr,
		closeCh: make(chan struct{}),
		drained: make(chan struct{}),
	}
	go s.demux(stdout)
	return s, nil
}

// demux reads Ogg pages from ffmpeg and turns them into frames. It exits on
// EOF, on a read error, or when the stream is closed.
func (s *stream) demux(stdout io.ReadCloser) {
	defer close(s.frames)
	defer close(s.drained)

	reader, _, err := oggreader.NewWith(stdout)
	if err != nil {
		s.fail(fmt.Errorf("read ogg stream: %w", err))
		return
	}

	var lastGranule uint64
	for {
		data, header, err := reader.ParseNextPage()
		if err != nil {
			if err != io.EOF {
				s.fail(err)
			}
			return
		}
		// The comment header carries no audio; granule position only advances
		// on pages that do.
		samples := header.GranulePosition - lastGranule
		lastGranule = header.GranulePosition
		if samples == 0 || len(data) == 0 {
			continue
		}
		frame := Frame{
			Data:     data,
			Duration: time.Duration(samples) * time.Second / rtc.SampleRate,
		}
		select {
		case s.frames <- frame:
		case <-s.closeCh:
			return
		}
	}
}

func (s *stream) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

// Err returns the first error the stream hit, including ffmpeg's own output.
func (s *stream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		return nil
	}
	if msg := strings.TrimSpace(s.stderr.String()); msg != "" {
		return fmt.Errorf("%w: %s", s.err, firstLine(msg))
	}
	return s.err
}

// Close kills ffmpeg and waits for the demuxer to stop. It is safe to call
// more than once and safe to call after the stream ended on its own.
func (s *stream) Close() {
	s.closeOnce.Do(func() {
		close(s.closeCh)
		s.cancel()
	})
	<-s.drained
	s.reap()
}

// reap waits for ffmpeg to exit, recording why. Killing ffmpeg ourselves is not
// an error, so Close's cancellation is filtered out here.
func (s *stream) reap() {
	s.waitOnce.Do(func() {
		err := s.cmd.Wait()
		if err == nil {
			return
		}
		select {
		case <-s.closeCh:
			// We killed it; that is not a failure.
			return
		default:
		}
		if msg := strings.TrimSpace(s.stderr.String()); msg != "" {
			err = fmt.Errorf("ffmpeg: %s", firstLine(msg))
		} else {
			err = fmt.Errorf("ffmpeg: %w", err)
		}
		s.mu.Lock()
		s.waitErr = err
		s.mu.Unlock()
	})
}

// finish is called once the frame channel has closed: it reaps ffmpeg and
// reports whether the track ended cleanly or failed.
func (s *stream) finish() error {
	s.reap()
	if err := s.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waitErr
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
