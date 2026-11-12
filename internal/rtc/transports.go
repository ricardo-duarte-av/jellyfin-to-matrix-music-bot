package rtc

import (
	"context"
	"net/http"

	"maunium.net/go/mautrix"
)

// RTCTransport is one entry of the homeserver's MatrixRTC transport registry
// (MSC4143).
type RTCTransport struct {
	Type string `json:"type"`
	// URL is the SFU's WebSocket address. A transport naming one, and no
	// service, is one the homeserver issues tokens for itself.
	URL string `json:"url,omitempty"`
	// LiveKitServiceURL names an authorization service instead.
	LiveKitServiceURL string `json:"livekit_service_url,omitempty"`
}

// respRTCTransports is the registry's response. The stable endpoint lists the
// transports under "transports"; earlier unstable ones used "rtc_transports".
type respRTCTransports struct {
	Transports    []RTCTransport `json:"transports"`
	RTCTransports []RTCTransport `json:"rtc_transports"`
}

// HomeserverSFU returns the SFU URL the homeserver issues tokens for through
// its own /rtc/livekit/get_token, or "" when it advertises none.
//
// The stable m.livekit entry wins over an unstable livekit one, the way
// matrix-js-sdk picks. A livekit entry naming a service as well as a URL does
// not count: clients treat it as the authorization service's, and so must the
// bot, or it publishes under an identity nobody looks for.
//
// The request is made through the mautrix client so it carries the access
// token: the endpoint is authenticated, and sending it unauthenticated is a
// mistake shipped clients have actually made (element-hq/element-call#3825).
func HomeserverSFU(ctx context.Context, client *mautrix.Client) (string, error) {
	paths := []mautrix.ClientURLPath{
		{"v1", "rtc", "transports"},
		{"unstable", "org.matrix.msc4143", "rtc", "transports"},
	}
	var lastErr error
	for _, path := range paths {
		var resp respRTCTransports
		_, err := client.MakeRequest(ctx, http.MethodGet, client.BuildURL(path), nil, &resp)
		if err != nil {
			if isEndpointMissing(err) {
				lastErr = err
				continue
			}
			return "", err
		}
		return pickSFU(append(resp.Transports, resp.RTCTransports...)), nil
	}
	if isEndpointMissing(lastErr) {
		return "", nil
	}
	return "", lastErr
}

// pickSFU chooses the transport HomeserverSFU reports.
func pickSFU(transports []RTCTransport) string {
	for _, typ := range []string{TransportTypeMLiveKit, TransportTypeLiveKit} {
		for _, t := range transports {
			if t.Type == typ && t.URL != "" && t.LiveKitServiceURL == "" {
				return t.URL
			}
		}
	}
	return ""
}
