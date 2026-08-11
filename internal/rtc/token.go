package rtc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
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

// GetSFUConfig exchanges a fresh Matrix OpenID token for a LiveKit JWT.
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

	cfg, err := postSFU(ctx, serviceURL, body)
	if err != nil && body.DelayID != "" {
		// Older services reject the delay fields with M_BAD_JSON. Retry
		// without them and fall back to refreshing the delay ourselves.
		body.DelayID, body.DelayCSAPIURL, body.DelayTimeout = "", "", 0
		cfg, err = postSFU(ctx, serviceURL, body)
	}
	if err != nil {
		return nil, err
	}

	alias, identity, err := parseJWT(cfg.JWT)
	if err != nil {
		return nil, err
	}
	cfg.Alias, cfg.Identity = alias, identity
	return cfg, nil
}

func postSFU(ctx context.Context, serviceURL string, body legacySFURequest) (*SFUConfig, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	url := strings.TrimSuffix(serviceURL, "/") + "/sfu/get"
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
		return nil, fmt.Errorf("post %s: status %s: %s", url, resp.Status, strings.TrimSpace(string(respBody)))
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
