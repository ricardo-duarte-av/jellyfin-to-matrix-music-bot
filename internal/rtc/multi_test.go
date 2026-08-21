package rtc

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// fakeLeg is one LiveKit connection that can be told to start failing.
type fakeLeg struct {
	identity string
	frames   int
	images   int
	videos   int
	closed   bool
	err      error
}

func (f *fakeLeg) WriteOpus(frame []byte) error {
	if f.err != nil {
		return f.err
	}
	f.frames++
	return nil
}

func (f *fakeLeg) ShowImage(keyframe []byte) error {
	if f.err != nil {
		return f.err
	}
	f.images++
	return nil
}

func (f *fakeLeg) PublishVideo(name string) error {
	if f.err != nil {
		return f.err
	}
	f.videos++
	return nil
}

func (f *fakeLeg) Identity() string { return f.identity }
func (f *fakeLeg) Close()           { f.closed = true }

func testMulti(legs ...*fakeLeg) (*MultiPublisher, []*leg) {
	wrapped := make([]*leg, len(legs))
	for i, l := range legs {
		wrapped[i] = &leg{name: fmt.Sprintf("leg%d", i), pub: l}
	}
	return newMultiPublisher(zerolog.New(io.Discard), wrapped...), wrapped
}

// The whole point of the second connection: both identities publish the same
// audio, so neither dialect's clients are left with a silent tile.
func TestWriteOpusReachesEveryLeg(t *testing.T) {
	a, b := &fakeLeg{}, &fakeLeg{}
	m, _ := testMulti(a, b)

	if err := m.WriteOpus([]byte{1, 2, 3}); err != nil {
		t.Fatalf("WriteOpus() = %v", err)
	}
	if a.frames != 1 || b.frames != 1 {
		t.Errorf("frames written: %d and %d; want 1 each", a.frames, b.frames)
	}
}

// One SFU connection going bad must not take the music away from the room.
func TestWriteOpusSurvivesAFailingLeg(t *testing.T) {
	good, bad := &fakeLeg{}, &fakeLeg{err: fmt.Errorf("connection reset")}
	m, wrapped := testMulti(good, bad)

	if err := m.WriteOpus([]byte{1}); err != nil {
		t.Fatalf("WriteOpus() = %v; want the surviving leg to carry it", err)
	}
	if good.frames != 1 {
		t.Errorf("good leg got %d frames; want 1", good.frames)
	}
	if !wrapped[1].dead {
		t.Error("failing leg was not retired")
	}

	// A retired leg is not tried again.
	bad.err = nil
	if err := m.WriteOpus([]byte{2}); err != nil {
		t.Fatalf("WriteOpus() = %v", err)
	}
	if bad.frames != 0 {
		t.Errorf("retired leg received %d frames; want none", bad.frames)
	}
}

// Losing every connection is a real failure and the player should hear about it.
func TestWriteOpusFailsWhenEveryLegIsGone(t *testing.T) {
	a := &fakeLeg{err: fmt.Errorf("gone")}
	m, _ := testMulti(a)

	if err := m.WriteOpus([]byte{1}); err == nil {
		t.Fatal("WriteOpus() = nil with no live connection")
	}
	if err := m.WriteOpus([]byte{2}); err == nil {
		t.Fatal("WriteOpus() = nil on the second attempt with no live connection")
	}
}

// Artwork is decorative: a connection that will not take a video track stays in
// the call as audio rather than failing the whole publish.
func TestPublishVideoToleratesOneLeg(t *testing.T) {
	good, bad := &fakeLeg{}, &fakeLeg{err: fmt.Errorf("no video")}
	m, _ := testMulti(good, bad)

	if err := m.PublishVideo("Jukebox"); err != nil {
		t.Fatalf("PublishVideo() = %v; want the working leg to be enough", err)
	}
	if good.videos != 1 {
		t.Errorf("good leg published %d video tracks; want 1", good.videos)
	}

	// No leg accepting it is a different matter.
	onlyBad, _ := testMulti(&fakeLeg{err: fmt.Errorf("no video")})
	if err := onlyBad.PublishVideo("Jukebox"); err == nil {
		t.Error("PublishVideo() = nil when no leg accepted the track")
	}
}

func TestIdentityNamesEveryLeg(t *testing.T) {
	m, _ := testMulti(&fakeLeg{identity: "@bot:example.org:DEVICE"}, &fakeLeg{identity: "hashed"})

	got := m.Identity()
	for _, want := range []string{"@bot:example.org:DEVICE", "hashed"} {
		if !strings.Contains(got, want) {
			t.Errorf("Identity() = %q; missing %q", got, want)
		}
	}
}

func TestCloseClosesEveryLeg(t *testing.T) {
	a, b := &fakeLeg{}, &fakeLeg{}
	m, _ := testMulti(a, b)

	m.Close()
	if !a.closed || !b.closed {
		t.Errorf("closed: %v and %v; want both", a.closed, b.closed)
	}
}
