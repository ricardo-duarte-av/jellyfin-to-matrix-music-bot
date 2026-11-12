package rtc

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"maunium.net/go/mautrix"
)

// The registry decides which flow the bot takes, so the choice has to match the
// one clients make from the same list.
func TestPickSFU(t *testing.T) {
	stable := RTCTransport{Type: TransportTypeMLiveKit, URL: "wss://stable.example.org"}
	unstable := RTCTransport{Type: TransportTypeLiveKit, URL: "wss://unstable.example.org"}
	service := RTCTransport{Type: TransportTypeLiveKit, LiveKitServiceURL: "https://jwt.example.org"}
	both := RTCTransport{Type: TransportTypeLiveKit, URL: "wss://both.example.org", LiveKitServiceURL: "https://jwt.example.org"}

	cases := []struct {
		name       string
		transports []RTCTransport
		want       string
	}{
		{"stable wins over unstable", []RTCTransport{unstable, stable}, stable.URL},
		{"unstable with only a url", []RTCTransport{service, unstable}, unstable.URL},
		{"service only", []RTCTransport{service}, ""},
		// Clients use the service's flow for a transport naming both.
		{"url alongside a service", []RTCTransport{both}, ""},
		{"none", nil, ""},
	}
	for _, c := range cases {
		if got := pickSFU(c.transports); got != c.want {
			t.Errorf("%s: pickSFU() = %q; want %q", c.name, got, c.want)
		}
	}
}

// Synapse's stable endpoint lists the transports under "transports", which is
// not where the unstable one put them.
func TestHomeserverSFUReadsTheStableRegistry(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/client/v1/rtc/transports" {
			writeJSON(w, http.StatusNotFound, map[string]any{"errcode": "M_UNRECOGNIZED"})
			return
		}
		auth = r.Header.Get("Authorization")
		writeJSON(w, http.StatusOK, map[string]any{"transports": []map[string]any{
			{"type": "m.livekit", "url": "wss://sfu.example.org:443"},
			{"type": "livekit", "livekit_service_url": "https://jwt.example.org"},
		}})
	}))
	t.Cleanup(srv.Close)
	client, err := mautrix.NewClient(srv.URL, "@bot:example.org", "token")
	if err != nil {
		t.Fatal(err)
	}

	got, err := HomeserverSFU(t.Context(), client)
	if err != nil {
		t.Fatalf("HomeserverSFU() = %v", err)
	}
	if got != "wss://sfu.example.org:443" {
		t.Errorf("HomeserverSFU() = %q; want the m.livekit url", got)
	}
	if auth != "Bearer token" {
		t.Errorf("registry asked with Authorization %q; it is an authenticated endpoint", auth)
	}
}

// A homeserver without the registry is not an error: it is the cue to use the
// authorization service.
func TestHomeserverSFUWithoutRegistry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{"errcode": "M_UNRECOGNIZED"})
	}))
	t.Cleanup(srv.Close)
	client, err := mautrix.NewClient(srv.URL, "@bot:example.org", "token")
	if err != nil {
		t.Fatal(err)
	}

	got, err := HomeserverSFU(t.Context(), client)
	if err != nil || got != "" {
		t.Errorf("HomeserverSFU() = %q, %v; want no SFU and no error", got, err)
	}
}

// matrix-js-sdk's rule: a url and no service is the homeserver's flow, anything
// naming a service is the service's.
func TestUsesHomeserverTokens(t *testing.T) {
	cases := []struct {
		t    Transport
		want bool
	}{
		{Transport{Type: TransportTypeMLiveKit, URL: "wss://sfu"}, true},
		{Transport{Type: TransportTypeLiveKit, URL: "wss://sfu"}, true},
		{Transport{Type: TransportTypeLiveKit, LiveKitServiceURL: "https://jwt"}, false},
		{Transport{Type: TransportTypeLiveKit, URL: "wss://sfu", LiveKitServiceURL: "https://jwt"}, false},
		{Transport{Type: TransportTypeLiveKit}, false},
		{Transport{Type: "other", URL: "wss://sfu"}, false},
	}
	for _, c := range cases {
		if got := UsesHomeserverTokens(c.t); got != c.want {
			t.Errorf("UsesHomeserverTokens(%+v) = %v; want %v", c.t, got, c.want)
		}
	}
}
