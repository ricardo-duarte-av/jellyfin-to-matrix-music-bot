package matrix

import (
	"reflect"
	"testing"
	"time"

	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/jellyfin"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		body   string
		want   Command
		wantOK bool
	}{
		{name: "simple", prefix: "!", body: "!pause", want: Command{Name: "pause", Args: []string{}, Rest: ""}, wantOK: true},
		{name: "with args", prefix: "!", body: "!play 1 2 3", want: Command{Name: "play", Args: []string{"1", "2", "3"}, Rest: "1 2 3"}, wantOK: true},
		{name: "query keeps spaces", prefix: "!", body: "!search dark side of the moon", want: Command{Name: "search", Args: []string{"dark", "side", "of", "the", "moon"}, Rest: "dark side of the moon"}, wantOK: true},
		{name: "uppercase name", prefix: "!", body: "!NEXT", want: Command{Name: "next", Args: []string{}, Rest: ""}, wantOK: true},
		{name: "surrounding space", prefix: "!", body: "  !next  ", want: Command{Name: "next", Args: []string{}, Rest: ""}, wantOK: true},
		{name: "custom prefix", prefix: ".", body: ".play 2", want: Command{Name: "play", Args: []string{"2"}, Rest: "2"}, wantOK: true},
		{name: "not a command", prefix: "!", body: "hello there", wantOK: false},
		{name: "wrong prefix", prefix: "!", body: ".play 2", wantOK: false},
		{name: "prefix only", prefix: "!", body: "!", wantOK: false},
		{name: "empty", prefix: "!", body: "", wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseCommand(tc.prefix, tc.body)
			if ok != tc.wantOK {
				t.Fatalf("ParseCommand(%q, %q) ok = %v; want %v", tc.prefix, tc.body, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseCommand(%q, %q) = %+v; want %+v", tc.prefix, tc.body, got, tc.want)
			}
		})
	}
}

func TestIntArgs(t *testing.T) {
	tests := []struct {
		args []string
		want []int
		ok   bool
	}{
		{args: []string{"1"}, want: []int{1}, ok: true},
		{args: []string{"3", "1", "2"}, want: []int{3, 1, 2}, ok: true},
		{args: []string{"dark", "side"}, ok: false},
		{args: []string{"1", "side"}, ok: false},
		{args: []string{"0"}, ok: false},
		{args: []string{"-2"}, ok: false},
		{args: nil, ok: false},
	}

	for _, tc := range tests {
		got, ok := intArgs(tc.args)
		if ok != tc.ok {
			t.Errorf("intArgs(%v) ok = %v; want %v", tc.args, ok, tc.ok)
			continue
		}
		if ok && !reflect.DeepEqual(got, tc.want) {
			t.Errorf("intArgs(%v) = %v; want %v", tc.args, got, tc.want)
		}
	}
}

func TestCanonicalName(t *testing.T) {
	aliases := map[string]string{
		"np": "nowplaying", "q": "queue", "s": "search", "p": "play",
		"n": "next", "back": "prev", "unpause": "resume", "ls": "list",
		"play": "play", "unknown": "unknown",
	}
	for in, want := range aliases {
		if got := canonicalName(in); got != want {
			t.Errorf("canonicalName(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestResultsResolve(t *testing.T) {
	r := newResults(time.Minute)

	if _, err := r.resolve(1); err == nil {
		t.Error("resolve() with no results succeeded; want an error")
	}

	items := []jellyfin.Item{
		{ID: "a", Name: "First", Kind: jellyfin.KindTrack},
		{ID: "b", Name: "Second", Kind: jellyfin.KindAlbum},
	}
	r.set(items)

	got, err := r.resolve(2)
	if err != nil {
		t.Fatalf("resolve(2) error: %v", err)
	}
	if got.ID != "b" {
		t.Errorf("resolve(2) = %q; want b — indexes are 1-based", got.ID)
	}
	for _, bad := range []int{0, 3, -1} {
		if _, err := r.resolve(bad); err == nil {
			t.Errorf("resolve(%d) succeeded; want an out-of-range error", bad)
		}
	}
}

func TestResultsExpire(t *testing.T) {
	r := newResults(10 * time.Millisecond)
	r.set([]jellyfin.Item{{ID: "a", Name: "First"}})

	if _, err := r.resolve(1); err != nil {
		t.Fatalf("resolve(1) immediately after set: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := r.resolve(1); err == nil {
		t.Error("resolve(1) succeeded after the TTL elapsed; want an error")
	}
	if got := r.get(); got != nil {
		t.Errorf("get() = %v after expiry; want nil", got)
	}
}

func TestBestMatchPrefersTrack(t *testing.T) {
	items := []jellyfin.Item{
		{ID: "artist", Kind: jellyfin.KindArtist},
		{ID: "album", Kind: jellyfin.KindAlbum},
		{ID: "track", Kind: jellyfin.KindTrack},
	}
	if got := bestMatch(items); got.ID != "track" {
		t.Errorf("bestMatch() = %q; want the track", got.ID)
	}

	noTracks := []jellyfin.Item{{ID: "artist", Kind: jellyfin.KindArtist}}
	if got := bestMatch(noTracks); got.ID != "artist" {
		t.Errorf("bestMatch() with no tracks = %q; want the first item", got.ID)
	}
}
