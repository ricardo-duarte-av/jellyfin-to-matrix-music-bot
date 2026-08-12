package rtc

import (
	"fmt"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// videoTrack publishes a single still image as an H.264 video track.
//
// A still needs exactly one keyframe: receivers hold the last decoded frame
// indefinitely, so there is nothing to send between changes. What the track
// must do is answer PLI (picture loss indication) — the SFU sends one whenever
// a subscriber joins or loses the stream, and without a fresh keyframe in reply
// that subscriber would sit looking at a blank tile.
type videoTrack struct {
	track *lksdk.LocalTrack
	pub   *lksdk.LocalTrackPublication

	mu        sync.Mutex
	keyframe  []byte
	lastSent  time.Time
	closed    bool
	stopTimer chan struct{}
}

func newVideoTrack(room *lksdk.Room, name string) (*videoTrack, error) {
	v := &videoTrack{stopTimer: make(chan struct{})}

	track, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeH264,
		ClockRate: 90000,
		// Constrained baseline, the profile every WebRTC stack offers. It has
		// to match one the SFU advertises or the track is rejected outright.
		SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
	}, lksdk.WithRTCPHandler(v.onRTCP))
	if err != nil {
		return nil, fmt.Errorf("create video track: %w", err)
	}

	pub, err := room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name:        name,
		Source:      livekit.TrackSource_CAMERA,
		VideoWidth:  videoSize,
		VideoHeight: videoSize,
	})
	if err != nil {
		return nil, fmt.Errorf("publish video track: %w", err)
	}

	v.track, v.pub = track, pub
	go v.refreshLoop()
	return v, nil
}

// videoSize must match what the renderer produces.
const videoSize = 720

// SetKeyframe replaces the displayed image and sends it immediately.
func (v *videoTrack) SetKeyframe(frame []byte) error {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return fmt.Errorf("video track is closed")
	}
	v.keyframe = frame
	v.mu.Unlock()
	return v.send()
}

// send writes the current keyframe, if there is one.
func (v *videoTrack) send() error {
	v.mu.Lock()
	frame, closed := v.keyframe, v.closed
	// Real elapsed time drives the RTP timestamp, so receivers see the frame
	// arrive when it actually did rather than as part of a steady stream.
	elapsed := time.Second
	if !v.lastSent.IsZero() {
		elapsed = time.Since(v.lastSent)
	}
	v.lastSent = time.Now()
	v.mu.Unlock()

	if closed || len(frame) == 0 {
		return nil
	}
	return v.track.WriteSample(media.Sample{Data: frame, Duration: elapsed}, nil)
}

// onRTCP answers keyframe requests from the SFU.
func (v *videoTrack) onRTCP(packet rtcp.Packet) {
	switch packet.(type) {
	case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
		_ = v.send()
	}
}

// refreshLoop re-sends the still periodically in case a PLI went missing.
func (v *videoTrack) refreshLoop() {
	ticker := time.NewTicker(videoRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-v.stopTimer:
			return
		case <-ticker.C:
			_ = v.send()
		}
	}
}

func (v *videoTrack) Close(room *lksdk.Room) {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return
	}
	v.closed = true
	v.keyframe = nil
	v.mu.Unlock()

	close(v.stopTimer)
	if room != nil && v.pub != nil {
		_ = room.LocalParticipant.UnpublishTrack(v.pub.SID())
	}
}
