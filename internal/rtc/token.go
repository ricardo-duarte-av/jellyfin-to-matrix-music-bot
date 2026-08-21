package rtc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

// TokenSource names which endpoint issued a token. The three form a ladder,
// newest first: the homeserver's own MSC4195 endpoint, the authorization
// service's MSC4195 endpoint, and the pre-Matrix-2.0 endpoint everything in
// the wild still speaks.
type TokenSource string

const (
	// TokenSourceHomeserver is POST /_matrix/client/v1/rtc/livekit/get_token.
	TokenSourceHomeserver TokenSource = "homeserver /get_token"
	// TokenSourceService is POST <authorization service>/get_token.
	TokenSourceService TokenSource = "service /get_token"
	// TokenSourceLegacy is POST <authorization service>/sfu/get.
	TokenSourceLegacy TokenSource = "service /sfu/get"
)

// SFUConfig is what the MatrixRTC authorization service hands back: where the
// LiveKit SFU is and the JWT to join it with.
type SFUConfig struct {
	URL string `json:"url"`
	JWT string `json:"jwt"`
	// Alias is the LiveKit room name, read back out of the JWT payload. The
	// service — not the client — decides the mapping from Matrix room to
	// LiveKit room, so this is the authoritative alias to advertise.
	Alias string `json:"-"`
	// Identity is the LiveKit participant identity the service assigned us.
	Identity string `json:"-"`
	// Source is the endpoint that issued this token.
	Source TokenSource `json:"-"`
}

// legacySFURequest is the body of POST /sfu/get. This endpoint derives the
// LiveKit identity as "<user_id>:<device_id>", which matches the membershipID
// convention of session-style membership events — the pair must stay
// consistent or other clients cannot map our audio to our membership.
type legacySFURequest struct {
	Room        string                   `json:"room"`
	OpenIDToken *mautrix.RespOpenIDToken `json:"openid_token"`
	DeviceID    string                   `json:"device_id"`

	// Optional delegation of the delayed leave event to the service, so the
	// bot is removed from the call even if it dies without warning.
	DelayID       string `json:"delay_id,omitempty"`
	DelayTimeout  int64  `json:"delay_timeout,omitempty"`
	DelayCSAPIURL string `json:"delay_cs_api_url,omitempty"`
}

// rtcMemberRef identifies which membership a token is being requested for.
// Under MSC4195 the LiveKit identity is derived from it, as
// base64(SHA256(["<user_id>", "<claimed_device_id>", "<member.id>"])), so
// these values must be exactly the ones in the published m.rtc.member event.
type rtcMemberRef struct {
	ID string `json:"id"`
	// ClaimedUserID is checked against the OpenID token's subject. It is only
	// sent to the authorization service; the homeserver already knows who we
	// are from the access token.
	ClaimedUserID   string `json:"claimed_user_id,omitempty"`
	ClaimedDeviceID string `json:"claimed_device_id"`
}

// homeserverTokenRequest is the body of the MSC4195 Client-Server endpoint.
// server_name is omitted: it defaults to the homeserver's own name, which is
// what we want, since we publish our media through our own SFU.
type homeserverTokenRequest struct {
	RoomID string       `json:"room_id"`
	SlotID string       `json:"slot_id"`
	Member rtcMemberRef `json:"member"`
}

// serviceTokenRequest is the body of the authorization service's /get_token.
// The service rejects unknown fields, so this must carry these keys and no
// others.
type serviceTokenRequest struct {
	RoomID      string                   `json:"room_id"`
	SlotID      string                   `json:"slot_id"`
	OpenIDToken *mautrix.RespOpenIDToken `json:"openid_token"`
	Member      rtcMemberRef             `json:"member"`
}

// GetSFUConfig exchanges a fresh Matrix OpenID token for a LiveKit JWT via the
// legacy /sfu/get endpoint. This is the token that carries the "<user>:<device>"
// identity session-style membership events point at.
//
// delayID/delayCSAPIURL are optional: when both are set, the service takes over
// restarting the delayed leave event while we are connected.
func GetSFUConfig(
	ctx context.Context,
	client *mautrix.Client,
	serviceURL string,
	roomID id.RoomID,
	deviceID string,
	delayID string,
	delayCSAPIURL string,
	delayTimeout time.Duration,
) (*SFUConfig, error) {
	openID, err := client.RequestOpenIDToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("request openid token: %w", err)
	}

	body := legacySFURequest{
		Room:        roomID.String(),
		OpenIDToken: openID,
		DeviceID:    deviceID,
	}
	if delayID != "" && delayCSAPIURL != "" {
		body.DelayID = delayID
		body.DelayCSAPIURL = delayCSAPIURL
		body.DelayTimeout = delayTimeout.Milliseconds()
	}

	cfg, err := postService(ctx, serviceURL, "/sfu/get", body)
	if err != nil && body.DelayID != "" {
		// Older services reject the delay fields with M_BAD_JSON. Retry
		// without them and fall back to refreshing the delay ourselves.
		body.DelayID, body.DelayCSAPIURL, body.DelayTimeout = "", "", 0
		cfg, err = postService(ctx, serviceURL, "/sfu/get", body)
	}
	if err != nil {
		return nil, err
	}
	return finishToken(cfg, TokenSourceLegacy)
}

// GetStickyToken obtains the LiveKit JWT for a sticky m.rtc.member membership.
//
// It tries the homeserver's own MSC4195 endpoint first and falls back to the
// authorization service's. Only a missing endpoint is a reason to fall through:
// a 401 or 500 is reported, because quietly downgrading a real failure hides
// the thing that actually needs fixing.
//
// It returns a nil config and nil error when neither endpoint exists, which is
// the caller's cue that this deployment cannot carry the sticky stack yet.
func GetStickyToken(
	ctx context.Context,
	client *mautrix.Client,
	serviceURL string,
	roomID id.RoomID,
	slotID string,
	memberID string,
	deviceID string,
) (*SFUConfig, error) {
	member := rtcMemberRef{ID: memberID, ClaimedDeviceID: deviceID}

	cfg, err := postHomeserverToken(ctx, client, homeserverTokenRequest{
		RoomID: roomID.String(),
		SlotID: slotID,
		Member: member,
	})
	if err == nil {
		return finishToken(cfg, TokenSourceHomeserver)
	}
	if !isEndpointMissing(err) {
		return nil, fmt.Errorf("homeserver get_token: %w", err)
	}
	client.Log.Debug().Msg("homeserver does not implement /rtc/livekit/get_token; trying the authorization service")

	if serviceURL == "" {
		return nil, nil
	}
	openID, err := client.RequestOpenIDToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("request openid token: %w", err)
	}
	member.ClaimedUserID = client.UserID.String()
	cfg, err = postService(ctx, serviceURL, "/get_token", serviceTokenRequest{
		RoomID:      roomID.String(),
		SlotID:      slotID,
		OpenIDToken: openID,
		Member:      member,
	})
	if err != nil {
		if isServiceEndpointMissing(err) {
			client.Log.Debug().Msg("authorization service does not implement /get_token")
			return nil, nil
		}
		return nil, err
	}
	return finishToken(cfg, TokenSourceService)
}

// finishToken fills in the fields that are only knowable from the JWT itself.
func finishToken(cfg *SFUConfig, source TokenSource) (*SFUConfig, error) {
	alias, identity, err := parseJWT(cfg.JWT)
	if err != nil {
		return nil, err
	}
	cfg.Alias, cfg.Identity, cfg.Source = alias, identity, source
	return cfg, nil
}

// postHomeserverToken calls the MSC4195 Client-Server endpoint. It goes through
// the mautrix client so the request carries the access token.
func postHomeserverToken(ctx context.Context, client *mautrix.Client, body homeserverTokenRequest) (*SFUConfig, error) {
	var cfg SFUConfig
	url := client.BuildURL(mautrix.ClientURLPath{"v1", "rtc", "livekit", "get_token"})
	if _, err := client.MakeRequest(ctx, http.MethodPost, url, &body, &cfg); err != nil {
		return nil, err
	}
	if cfg.URL == "" || cfg.JWT == "" {
		return nil, fmt.Errorf("get_token response missing url or jwt")
	}
	return &cfg, nil
}

// postService posts to one of the authorization service's endpoints.
func postService(ctx context.Context, serviceURL, path string, body any) (*SFUConfig, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	url := strings.TrimSuffix(serviceURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post %s: %w", url, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, &serviceError{
			URL:    url,
			Status: resp.Status,
			Code:   resp.StatusCode,
			Body:   strings.TrimSpace(string(respBody)),
		}
	}
	var cfg SFUConfig
	if err := json.Unmarshal(respBody, &cfg); err != nil {
		return nil, fmt.Errorf("parse sfu response: %w", err)
	}
	if cfg.URL == "" || cfg.JWT == "" {
		return nil, fmt.Errorf("sfu response missing url or jwt")
	}
	return &cfg, nil
}

// serviceError is a non-2xx response from the authorization service. The status
// code is kept so callers can tell "you do not implement this" apart from
// "your answer is no".
type serviceError struct {
	URL    string
	Status string
	Code   int
	Body   string
}

func (e *serviceError) Error() string {
	return fmt.Sprintf("post %s: status %s: %s", e.URL, e.Status, e.Body)
}

// isServiceEndpointMissing reports whether the service simply does not serve
// this path, as opposed to having refused the request.
func isServiceEndpointMissing(err error) bool {
	var svcErr *serviceError
	if !errors.As(err, &svcErr) {
		return false
	}
	return svcErr.Code == http.StatusNotFound || svcErr.Code == http.StatusMethodNotAllowed
}

// parseJWT pulls the LiveKit room name and participant identity out of the
// JWT's claims without verifying it — the SFU does the verifying; we only need
// to know what the service decided.
func parseJWT(token string) (alias, identity string, err error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", "", fmt.Errorf("malformed livekit jwt")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", fmt.Errorf("decode livekit jwt payload: %w", err)
	}
	var claims struct {
		Sub   string `json:"sub"`
		Video struct {
			Room string `json:"room"`
		} `json:"video"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return "", "", fmt.Errorf("parse livekit jwt payload: %w", err)
	}
	return claims.Video.Room, claims.Sub, nil
}
