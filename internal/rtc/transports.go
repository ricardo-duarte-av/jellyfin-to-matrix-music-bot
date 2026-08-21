package rtc

import (
	"context"
	"net/http"

	"maunium.net/go/mautrix"
)

// RTCTransport is one entry of the homeserver's MatrixRTC transport registry
// (MSC4519). Unlike the .well-known foci it carries no URL: under MSC4195 the
// token — and with it the SFU address — comes from the homeserver itself.
type RTCTransport struct {
	Type string `json:"type"`
}

type respRTCTransports struct {
	RTCTransports []RTCTransport `json:"rtc_transports"`
}

// HomeserverHasLiveKitTransport reports whether the homeserver advertises a
// livekit transport through the MSC4143 registry endpoint.
//
// The request is made through the mautrix client so it carries the access
// token: the endpoint is authenticated, and sending it unauthenticated is a
// mistake shipped clients have actually made (element-hq/element-call#3825).
func HomeserverHasLiveKitTransport(ctx context.Context, client *mautrix.Client) (bool, error) {
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
			return false, err
		}
		for _, transport := range resp.RTCTransports {
			if transport.Type == TransportTypeLiveKit {
				return true, nil
			}
		}
		return false, nil
	}
	return false, lastErr
}
