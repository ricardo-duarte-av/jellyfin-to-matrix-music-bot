// Package rtc implements the MatrixRTC side of the bot: discovering the
// MatrixRTC authorization service, exchanging a Matrix OpenID token for a
// LiveKit JWT, announcing call membership in the room, and publishing audio to
// the LiveKit SFU.
package rtc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// TransportTypeLiveKit is the LiveKit transport of earlier MSC4195 drafts. It
// names an authorization service (livekit_service_url), and the identity it
// implies is whatever that service's token carries.
const TransportTypeLiveKit = "livekit"

// TransportTypeMLiveKit is the stable MSC4195 LiveKit transport. It names the
// SFU itself (url); tokens for it come from the member's homeserver, and
// clients look for the identity base64(sha256([user_id, member.id])).
const TransportTypeMLiveKit = "m.livekit"

// Transport is a MatrixRTC "focus" / transport descriptor as it appears in
// call membership events and in .well-known.
type Transport struct {
	Type string `json:"type"`
	// URL is the SFU's WebSocket address, which a stable m.livekit transport
	// carries in place of a service.
	URL string `json:"url,omitempty"`
	// LiveKitServiceURL is the MatrixRTC authorization service (lk-jwt-service).
	LiveKitServiceURL string `json:"livekit_service_url,omitempty"`
	// LiveKitAlias is the SFU room alias, normally the Matrix room ID.
	LiveKitAlias string `json:"livekit_alias,omitempty"`
}

// UsesHomeserverTokens reports whether a member publishing t takes its tokens
// from its homeserver: a LiveKit transport naming the SFU and no service. This
// is matrix-js-sdk's rule; a transport naming both is the service's.
func UsesHomeserverTokens(t Transport) bool {
	return (t.Type == TransportTypeMLiveKit || t.Type == TransportTypeLiveKit) &&
		t.URL != "" && t.LiveKitServiceURL == ""
}

// FocusSelection is the deprecated-but-still-required focus_active field of a
// session-style membership event.
type FocusSelection struct {
	Type string `json:"type"`
	// FocusSelection is "oldest_membership": everyone uses the transport
	// proposed by whoever joined the call first.
	FocusSelection string `json:"focus_selection"`
}

type wellKnown struct {
	RTCFoci []Transport `json:"org.matrix.msc4143.rtc_foci"`
}

// DiscoverService returns the MatrixRTC authorization service URL advertised by
// the homeserver's /.well-known/matrix/client.
func DiscoverService(ctx context.Context, homeserver string) (string, error) {
	url := homeserver + "/.well-known/matrix/client"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch %s: unexpected status %s", url, resp.Status)
	}
	var wk wellKnown
	if err := json.NewDecoder(resp.Body).Decode(&wk); err != nil {
		return "", fmt.Errorf("parse %s: %w", url, err)
	}
	for _, focus := range wk.RTCFoci {
		if focus.Type == TransportTypeLiveKit && focus.LiveKitServiceURL != "" {
			return focus.LiveKitServiceURL, nil
		}
	}
	return "", fmt.Errorf("no livekit MatrixRTC focus advertised in %s; set rtc.livekit_service_url in config.yaml", url)
}
