package rtc

import (
	"fmt"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

const (
	// SampleRate is what we ask ffmpeg to encode Opus at. 48kHz is Opus's
	// native rate and what every WebRTC client expects.
	SampleRate = 48000

	// FrameDuration is the Opus frame size the bot publishes. ffmpeg is told to
	// use the same value so one Ogg packet is one RTP sample.
	FrameDuration = 20 * time.Millisecond
)

// Publisher owns the LiveKit connection and the bot's outgoing audio track.
//
// The bot publishes pre-encoded Opus produced by ffmpeg rather than raw PCM:
// LiveKit's PCM helper encodes via cgo bindings to libopus and libsoxr, and
// since ffmpeg is already a hard dependency for decoding, letting it encode too
// keeps the bot a pure-Go, dependency-light binary.
type Publisher struct {
	mu    sync.Mutex
	room  *lksdk.Room
	track *lksdk.LocalTrack
	pub   *lksdk.LocalTrackPublication

	video *videoTrack
}

// videoRefresh is the safety net for the still image: subscribers normally get
// a keyframe on demand via PLI, so this only covers the case where a PLI is
// lost or never sent.
const videoRefresh = 10 * time.Second

// Connect joins the LiveKit room described by cfg and publishes an audio track
// that the player writes Opus frames into.
//
// channels must match what the encoder produces. It is negotiated in three
// places that have to agree, or listeners get a downmix: the RTP codec
// parameters, the SDP fmtp offered to subscribers, and the AddTrackRequest the
// SFU uses when describing the track onwards.
func Connect(cfg *SFUConfig, displayName string, channels int) (*Publisher, error) {
	room, err := lksdk.ConnectToRoomWithToken(cfg.URL, cfg.JWT, &lksdk.RoomCallback{},
		lksdk.WithAutoSubscribe(false))
	if err != nil {
		return nil, fmt.Errorf("connect to livekit at %s: %w", cfg.URL, err)
	}

	stereo := channels == 2
	fmtp := "minptime=10;useinbandfec=1"
	if stereo {
		fmtp += ";stereo=1;sprop-stereo=1"
	}

	track, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{
		MimeType:    webrtc.MimeTypeOpus,
		ClockRate:   SampleRate,
		Channels:    uint16(channels),
		SDPFmtpLine: fmtp,
	})
	if err != nil {
		room.Disconnect()
		return nil, fmt.Errorf("create audio track: %w", err)
	}

	pub, err := room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name:   displayName,
		Source: livekit.TrackSource_MICROPHONE,
		Stereo: stereo,
		// The encoder never emits DTX, so do not let the SFU advertise it.
		DisableDTX: true,
	})
	if err != nil {
		room.Disconnect()
		return nil, fmt.Errorf("publish audio track: %w", err)
	}

	return &Publisher{room: room, track: track, pub: pub}, nil
}

// WriteOpus sends one encoded Opus frame to the SFU. The caller is responsible
// for pacing: this does not block for the frame's duration.
func (p *Publisher) WriteOpus(frame []byte) error {
	p.mu.Lock()
	track := p.track
	p.mu.Unlock()
	if track == nil {
		return fmt.Errorf("publisher is closed")
	}
	return track.WriteSample(media.Sample{Data: frame, Duration: FrameDuration}, nil)
}

// PublishVideo adds a video track carrying a still image. It is optional: the
// bot works as an audio-only participant if this fails or is never called.
func (p *Publisher) PublishVideo(name string) error {
	p.mu.Lock()
	room := p.room
	if room == nil {
		p.mu.Unlock()
		return fmt.Errorf("publisher is closed")
	}
	if p.video != nil {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()

	video, err := newVideoTrack(room, name)
	if err != nil {
		return err
	}

	p.mu.Lock()
	p.video = video
	p.mu.Unlock()
	return nil
}

// ShowImage displays an H.264 keyframe on the video track. It is a no-op when no
// video track has been published.
func (p *Publisher) ShowImage(keyframe []byte) error {
	p.mu.Lock()
	video := p.video
	p.mu.Unlock()
	if video == nil {
		return nil
	}
	return video.SetKeyframe(keyframe)
}

// Identity is the LiveKit participant identity in use.
func (p *Publisher) Identity() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.room == nil {
		return ""
	}
	return p.room.LocalParticipant.Identity()
}

// Close unpublishes the track and disconnects from the SFU.
func (p *Publisher) Close() {
	p.mu.Lock()
	room, pub, video := p.room, p.pub, p.video
	p.track, p.room, p.pub, p.video = nil, nil, nil, nil
	p.mu.Unlock()

	if video != nil {
		video.Close(room)
	}
	if room != nil {
		if pub != nil {
			_ = room.LocalParticipant.UnpublishTrack(pub.SID())
		}
		room.Disconnect()
	}
}
