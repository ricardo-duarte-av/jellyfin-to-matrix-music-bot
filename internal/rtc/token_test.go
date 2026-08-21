package rtc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

// testJWT builds a LiveKit JWT with just the claims parseJWT reads.
func testJWT(t *testing.T, alias, identity string) string {
	t.Helper()
	claims := map[string]any{"sub": identity, "video": map[string]any{"room": alias}}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

// homeserver stands in for both the Matrix homeserver and, on the paths it does
// not serve, a homeserver that has never heard of MSC4195.
type homeserver struct {
	t *testing.T
	// getToken handles /_matrix/client/v1/rtc/livekit/get_token. Nil means the
	// endpoint does not exist.
	getToken http.HandlerFunc
	// lastTokenBody is the last body posted to that endpoint.
	lastTokenBody map[string]any
}

func (h *homeserver) client(t *testing.T) (*mautrix.Client, *httptest.Server) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/_matrix/client/v3/user/{userID}/openid/request_token",
		func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{
				"access_token":       "openid-token",
				"token_type":         "Bearer",
				"matrix_server_name": "example.org",
				"expires_in":         3600,
			})
		})
	mux.HandleFunc("/_matrix/client/v1/rtc/livekit/get_token", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&h.lastTokenBody)
		if h.getToken == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"errcode": "M_UNRECOGNIZED", "error": "Unrecognized request",
			})
			return
		}
		h.getToken(w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		h.t.Errorf("unexpected homeserver request to %s", r.URL.Path)
		writeJSON(w, http.StatusNotFound, map[string]any{"errcode": "M_UNRECOGNIZED"})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client, err := mautrix.NewClient(srv.URL, "@bot:example.org", "token")
	if err != nil {
		t.Fatal(err)
	}
	return client, srv
}

// service stands in for lk-jwt-service.
type service struct {
	getToken http.HandlerFunc
	sfuGet   http.HandlerFunc
	lastPath string
	lastBody map[string]any
}

func (s *service) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.lastPath = r.URL.Path
		s.lastBody = nil
		_ = json.NewDecoder(r.Body).Decode(&s.lastBody)
		var handler http.HandlerFunc
		switch r.URL.Path {
		case "/get_token":
			handler = s.getToken
		case "/sfu/get":
			handler = s.sfuGet
		}
		if handler == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"errcode": "M_UNRECOGNIZED"})
			return
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func tokenResponse(t *testing.T, alias, identity string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"url": "wss://sfu.example.org", "jwt": testJWT(t, alias, identity),
		})
	}
}

// The homeserver's own endpoint is the top rung: when it answers, nothing else
// is asked, and in particular no OpenID token is minted.
func TestStickyTokenPrefersTheHomeserver(t *testing.T) {
	hs := &homeserver{t: t, getToken: tokenResponse(t, "alias", "hashed-identity")}
	client, _ := hs.client(t)
	svc := &service{}
	serviceURL := svc.start(t)

	cfg, err := GetStickyToken(context.Background(), client, serviceURL,
		id.RoomID("!room:example.org"), DefaultSlotID, "member-id", "DEVICE")
	if err != nil {
		t.Fatalf("GetStickyToken() = %v", err)
	}
	if cfg.Source != TokenSourceHomeserver {
		t.Errorf("Source = %q; want %q", cfg.Source, TokenSourceHomeserver)
	}
	if cfg.Alias != "alias" || cfg.Identity != "hashed-identity" {
		t.Errorf("alias/identity = %q/%q; want alias/hashed-identity", cfg.Alias, cfg.Identity)
	}
	if svc.lastPath != "" {
		t.Errorf("authorization service was contacted at %q despite the homeserver answering", svc.lastPath)
	}

	// The identity the SFU assigns is a hash over these three values, so they
	// must be exactly what the published membership carries.
	member, _ := hs.lastTokenBody["member"].(map[string]any)
	if member["id"] != "member-id" || member["claimed_device_id"] != "DEVICE" {
		t.Errorf("member = %+v; want id=member-id claimed_device_id=DEVICE", member)
	}
	if hs.lastTokenBody["slot_id"] != DefaultSlotID {
		t.Errorf("slot_id = %v; want %q", hs.lastTokenBody["slot_id"], DefaultSlotID)
	}
	if _, ok := hs.lastTokenBody["server_name"]; ok {
		t.Error("server_name was sent; it should default to our own homeserver")
	}
}

// A homeserver that does not implement the endpoint is not a failure — that is
// every homeserver today — so the request moves down to the service.
func TestStickyTokenFallsThroughToTheService(t *testing.T) {
	hs := &homeserver{t: t}
	client, _ := hs.client(t)
	svc := &service{getToken: tokenResponse(t, "alias", "hashed-identity")}
	serviceURL := svc.start(t)

	cfg, err := GetStickyToken(context.Background(), client, serviceURL,
		id.RoomID("!room:example.org"), DefaultSlotID, "member-id", "DEVICE")
	if err != nil {
		t.Fatalf("GetStickyToken() = %v", err)
	}
	if cfg.Source != TokenSourceService {
		t.Errorf("Source = %q; want %q", cfg.Source, TokenSourceService)
	}

	// The service rejects unknown fields outright, so the body must carry
	// these keys and no others.
	want := map[string]bool{"room_id": true, "slot_id": true, "openid_token": true, "member": true}
	for key := range svc.lastBody {
		if !want[key] {
			t.Errorf("unexpected field %q in /get_token body", key)
		}
	}
	for key := range want {
		if _, ok := svc.lastBody[key]; !ok {
			t.Errorf("missing field %q in /get_token body", key)
		}
	}
	member, _ := svc.lastBody["member"].(map[string]any)
	if member["claimed_user_id"] != "@bot:example.org" {
		t.Errorf("claimed_user_id = %v; want @bot:example.org", member["claimed_user_id"])
	}
}

// Neither endpoint existing is a deployment that cannot carry the sticky stack,
// which the caller has to be able to tell apart from an error.
func TestStickyTokenReportsNoEndpoint(t *testing.T) {
	hs := &homeserver{t: t}
	client, _ := hs.client(t)
	svc := &service{}
	serviceURL := svc.start(t)

	cfg, err := GetStickyToken(context.Background(), client, serviceURL,
		id.RoomID("!room:example.org"), DefaultSlotID, "member-id", "DEVICE")
	if err != nil {
		t.Fatalf("GetStickyToken() = %v; want no error", err)
	}
	if cfg != nil {
		t.Errorf("GetStickyToken() = %+v; want nil", cfg)
	}
}

// A refusal is not a missing endpoint. Falling through on a 401 would hide the
// thing that actually needs fixing behind a silent downgrade.
func TestStickyTokenDoesNotFallThroughOnRefusal(t *testing.T) {
	hs := &homeserver{t: t, getToken: func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"errcode": "M_UNAUTHORIZED", "error": "The request could not be authorised.",
		})
	}}
	client, _ := hs.client(t)
	svc := &service{getToken: tokenResponse(t, "alias", "identity")}
	serviceURL := svc.start(t)

	if _, err := GetStickyToken(context.Background(), client, serviceURL,
		id.RoomID("!room:example.org"), DefaultSlotID, "member-id", "DEVICE"); err == nil {
		t.Fatal("GetStickyToken() = nil error; want the 401 reported")
	}
	if svc.lastPath != "" {
		t.Errorf("fell through to %q after a 401", svc.lastPath)
	}
}

// The legacy endpoint is untouched by the ladder: it still derives the
// "<user>:<device>" identity the session-style membership points at.
func TestLegacySFUGetIsUnchanged(t *testing.T) {
	hs := &homeserver{t: t}
	client, _ := hs.client(t)
	svc := &service{sfuGet: tokenResponse(t, "alias", "@bot:example.org:DEVICE")}
	serviceURL := svc.start(t)

	cfg, err := GetSFUConfig(context.Background(), client, serviceURL,
		id.RoomID("!room:example.org"), "DEVICE", "", "", 0)
	if err != nil {
		t.Fatalf("GetSFUConfig() = %v", err)
	}
	if cfg.Source != TokenSourceLegacy {
		t.Errorf("Source = %q; want %q", cfg.Source, TokenSourceLegacy)
	}
	if cfg.Identity != "@bot:example.org:DEVICE" {
		t.Errorf("Identity = %q; unexpected", cfg.Identity)
	}
	if svc.lastPath != "/sfu/get" {
		t.Errorf("posted to %q; want /sfu/get", svc.lastPath)
	}
	if svc.lastBody["device_id"] != "DEVICE" || svc.lastBody["room"] != "!room:example.org" {
		t.Errorf("body = %+v; want room and device_id", svc.lastBody)
	}
}

func TestParseJWT(t *testing.T) {
	alias, identity, err := parseJWT(testJWT(t, "the-alias", "the-identity"))
	if err != nil {
		t.Fatalf("parseJWT() = %v", err)
	}
	if alias != "the-alias" || identity != "the-identity" {
		t.Errorf("parseJWT() = %q, %q; want the-alias, the-identity", alias, identity)
	}
	if _, _, err := parseJWT("not-a-jwt"); err == nil {
		t.Error("parseJWT() accepted a malformed token")
	}
}
