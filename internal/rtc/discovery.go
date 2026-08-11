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

// TransportTypeLiveKit is the only RTC transport this bot supports.
const TransportTypeLiveKit = "livekit"

// Transport is a MatrixRTC "focus" / transport descriptor as it appears in
// call membership events and in .well-known.
type Transport struct {
	Type string `json:"type"`
	// LiveKitServiceURL is the MatrixRTC authorization service (lk-jwt-service).
	LiveKitServiceURL string `json:"livekit_service_url,omitempty"`
	// LiveKitAlias is the SFU room alias, normally the Matrix room ID.
	LiveKitAlias string `json:"livekit_alias,omitempty"`
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
