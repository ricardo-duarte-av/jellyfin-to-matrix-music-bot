package jellyfin

import (
	"strings"
	"testing"
	"time"
)

// A play is recorded on the same rule Jellyfin's own clients use, so the bot's
// play counts do not drift from the rest of the library.
func TestCountsAsPlayed(t *testing.T) {
	track := Item{ID: "a", Kind: KindTrack, Duration: 100 * time.Second}

	tests := []struct {
		name    string
		item    Item
		elapsed time.Duration
		want    bool
	}{
		{name: "finished", item: track, elapsed: 100 * time.Second, want: true},
		{name: "at the threshold", item: track, elapsed: 90 * time.Second, want: true},
		{name: "just short", item: track, elapsed: 89 * time.Second, want: false},
		{name: "skipped early", item: track, elapsed: 3 * time.Second, want: false},
		{name: "never started", item: track, elapsed: 0, want: false},
		{name: "negative", item: track, elapsed: -time.Second, want: false},
		// Without a duration there is nothing to measure against, so anything
		// that played counts rather than nothing ever counting.
		{name: "unknown duration", item: Item{ID: "b"}, elapsed: time.Second, want: true},
		{name: "unknown duration, nothing played", item: Item{ID: "b"}, elapsed: 0, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CountsAsPlayed(tc.item, tc.elapsed); got != tc.want {
				t.Errorf("CountsAsPlayed(%v, %v) = %v; want %v", tc.item.ID, tc.elapsed, got, tc.want)
			}
		})
	}
}

// Jellyfin measures time in 100ns ticks.
func TestTicks(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want int64
	}{
		{in: 0, want: 0},
		{in: time.Second, want: ticksPerSecond},
		{in: 90 * time.Second, want: 90 * ticksPerSecond},
		{in: -time.Second, want: 0},
	} {
		if got := ticks(tc.in); got != tc.want {
			t.Errorf("ticks(%v) = %d; want %d", tc.in, got, tc.want)
		}
	}
}

// Jellyfin opens a session from the device fields in the authorization header
// and rejects the request when they are missing, so every one has to be there.
func TestAuthHeaderCarriesTheDeviceFields(t *testing.T) {
	got := authHeader("secret", "abc123")
	for _, want := range []string{
		`Client="Jellyfin-to-Matrix"`,
		`Device="Jellyfin-to-Matrix"`,
		`DeviceId="abc123"`,
		`Version="1.0.0"`,
		`Token="secret"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("auth header %q is missing %s", got, want)
		}
	}
}

// A restarted bot must land on the same session rather than leaving a dead one
// behind, so the device ID cannot be random.
func TestDeviceIDIsStable(t *testing.T) {
	first := deviceID("https://jellyfin.example.org", "user")
	if second := deviceID("https://jellyfin.example.org", "user"); first != second {
		t.Errorf("deviceID() = %q then %q; want the same value across restarts", first, second)
	}
	if other := deviceID("https://other.example.org", "user"); other == first {
		t.Error("deviceID() ignores the server; two bots would share one session")
	}
}
