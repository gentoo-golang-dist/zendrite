// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only.

package routing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codefloe.com/pat-s/zendrite/clientapi/auth/authtypes"
	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/httputil"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
	"codefloe.com/pat-s/zendrite/userapi"
	userapiAPI "codefloe.com/pat-s/zendrite/userapi/api"
)

// setupMSC3861SSORouters creates a full router stack with MSC3861 enabled and
// the provided OIDC config. It is used by the legacy SSO compat tests because
// those need to point the homeserver at a mock OIDC provider.
func setupMSC3861SSORouters(t *testing.T, msc3861 config.MSC3861Config) (httputil.Routers, *config.Zendrite, func()) {
	t.Helper()
	cfg, processCtx, closeDB := testrig.CreateConfig(t, test.DBTypeSQLite)
	cfg.ClientAPI.RateLimiting.Enabled = false
	cfg.MSCs.MSCs = []string{"msc3861"}
	cfg.MSCs.MSC3861 = msc3861
	cfg.Global.WellKnownClientName = "https://matrix.example.com"

	natsInstance := jetstream.NATSInstance{}
	cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
	caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
	rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
	rsAPI.SetFederationAPI(nil, nil)
	uapi := userapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, rsAPI, nil, caching.DisableMetrics, testIsBlacklistedOrBackingOff)

	routers := httputil.NewRouters()
	Setup(routers, cfg, nil, nil, uapi, nil, nil, nil, nil, nil, nil, nil, nil, caching.DisableMetrics)

	return routers, cfg, closeDB
}

func TestMSC3861SSOCompat_LoginFlowsAdvertiseSSOAndToken(t *testing.T) {
	t.Parallel()
	routers, _, closeDB := setupMSC3861SSORouters(t, config.MSC3861Config{
		Issuer:       "https://auth.example.com",
		ClientID:     "test-client-id",
		ClientSecret: "test-client-secret",
	})
	defer closeDB()

	req := test.NewRequest(t, http.MethodGet, "/_matrix/client/v3/login")
	rec := httptest.NewRecorder()
	routers.Client.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp flows
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(resp.Flows) != 2 {
		t.Fatalf("expected 2 flows, got %d: %+v", len(resp.Flows), resp.Flows)
	}
	got := make(map[string]bool)
	for _, f := range resp.Flows {
		got[f.Type] = true
		if f.Type == authtypes.LoginTypeSSO && !f.OAuthAwarePreferred {
			t.Errorf("expected SSO flow to be marked oauth_aware_preferred")
		}
	}
	if !got[authtypes.LoginTypeSSO] {
		t.Errorf("expected %s flow", authtypes.LoginTypeSSO)
	}
	if !got[authtypes.LoginTypeToken] {
		t.Errorf("expected %s flow", authtypes.LoginTypeToken)
	}
}

func TestMSC3861SSOCompat_LoginPOSTPasswordForbidden(t *testing.T) {
	t.Parallel()
	routers, _, closeDB := setupMSC3861SSORouters(t, config.MSC3861Config{
		Issuer:       "https://auth.example.com",
		ClientID:     "test-client-id",
		ClientSecret: "test-client-secret",
	})
	defer closeDB()

	req := test.NewRequest(t, http.MethodPost, "/_matrix/client/v3/login", test.WithJSONBody(t, map[string]any{
		"type":     authtypes.LoginTypePassword,
		"user":     "alice",
		"password": "secret",
	}))
	rec := httptest.NewRecorder()
	routers.Client.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for password login, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMSC3861SSOCompat_LoginPOSTTokenAllowed(t *testing.T) {
	t.Parallel()
	cfg, processCtx, closeDB := testrig.CreateConfig(t, test.DBTypeSQLite)
	defer closeDB()
	cfg.ClientAPI.RateLimiting.Enabled = false
	cfg.MSCs.MSCs = []string{"msc3861"}
	cfg.MSCs.MSC3861 = config.MSC3861Config{
		Issuer:                "https://auth.example.com",
		ClientID:              "test-client-id",
		ClientSecret:          "test-client-secret",
		IntrospectionEndpoint: "https://auth.example.com/introspect",
	}
	cfg.Global.WellKnownClientName = "https://matrix.example.com"

	natsInstance := jetstream.NATSInstance{}
	cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
	caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
	rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
	rsAPI.SetFederationAPI(nil, nil)
	uapi := userapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, rsAPI, nil, caching.DisableMetrics, testIsBlacklistedOrBackingOff)

	// Create the account first, because QueryLoginToken validates it exists.
	var createRes userapiAPI.PerformAccountCreationResponse
	if err := uapi.PerformAccountCreation(context.Background(), &userapiAPI.PerformAccountCreationRequest{
		Localpart:  "alice",
		ServerName: cfg.Global.ServerName,
	}, &createRes); err != nil {
		t.Fatalf("failed to create account: %v", err)
	}

	// Create a login token manually.
	var tokenRes userapiAPI.PerformLoginTokenCreationResponse
	if err := uapi.PerformLoginTokenCreation(context.Background(), &userapiAPI.PerformLoginTokenCreationRequest{
		Data: userapiAPI.LoginTokenData{UserID: "@alice:" + string(cfg.Global.ServerName)},
	}, &tokenRes); err != nil {
		t.Fatalf("failed to create login token: %v", err)
	}

	routers := httputil.NewRouters()
	Setup(routers, cfg, nil, nil, uapi, nil, nil, nil, nil, nil, nil, nil, nil, caching.DisableMetrics)

	req := test.NewRequest(t, http.MethodPost, "/_matrix/client/v3/login", test.WithJSONBody(t, map[string]any{
		"type":  authtypes.LoginTypeToken,
		"token": tokenRes.Metadata.Token,
	}))
	rec := httptest.NewRecorder()
	routers.Client.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for token login, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp loginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse login response: %v", err)
	}
	if !strings.HasPrefix(resp.UserID, "@alice:") {
		t.Errorf("expected user_id @alice:..., got %s", resp.UserID)
	}
	if resp.AccessToken == "" {
		t.Error("expected access_token")
	}
}

func TestMSC3861SSOCompat_RedirectUsesConfiguredEndpoints(t *testing.T) {
	t.Parallel()
	authzEndpoint := "https://auth.example.com/custom/auth"
	tokenEndpoint := "https://auth.example.com/custom/token"

	routers, _, closeDB := setupMSC3861SSORouters(t, config.MSC3861Config{
		Issuer:                "https://auth.example.com",
		ClientID:              "test-client-id",
		ClientSecret:          "test-client-secret",
		AuthorizationEndpoint: authzEndpoint,
		TokenEndpoint:         tokenEndpoint,
		SSOCallbackURL:        "https://matrix.example.com/_matrix/client/v3/login/sso/callback",
	})
	defer closeDB()

	req := test.NewRequest(t, http.MethodGet, "/_matrix/client/v3/login/sso/redirect", test.WithQueryParams(map[string]string{
		"redirectUrl": "element://vector/webapp",
	}))
	rec := httptest.NewRecorder()
	routers.Client.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d: %s", rec.Code, rec.Body.String())
	}

	location := rec.Header().Get("Location")
	if location == "" {
		t.Fatal("expected Location header")
	}
	if !strings.HasPrefix(location, authzEndpoint+"?") {
		t.Fatalf("expected redirect to configured authorization endpoint, got %s", location)
	}

	u, err := url.Parse(location)
	if err != nil {
		t.Fatalf("failed to parse Location: %v", err)
	}
	q := u.Query()
	if q.Get("client_id") != "test-client-id" {
		t.Errorf("expected client_id test-client-id, got %s", q.Get("client_id"))
	}
	if q.Get("response_type") != "code" {
		t.Errorf("expected response_type code, got %s", q.Get("response_type"))
	}
	if q.Get("redirect_uri") != "https://matrix.example.com/_matrix/client/v3/login/sso/callback" {
		t.Errorf("unexpected redirect_uri: %s", q.Get("redirect_uri"))
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("expected PKCE S256, got %s", q.Get("code_challenge_method"))
	}
	if q.Get("code_challenge") == "" {
		t.Error("expected code_challenge")
	}
	if q.Get("state") == "" {
		t.Error("expected state")
	}
	// A device ID must be allocated and requested via the stable device scope.
	scope := q.Get("scope")
	if !strings.HasPrefix(scope, "openid urn:matrix:client:api:* urn:matrix:client:device:") {
		t.Errorf("expected scope to request a device ID, got %q", scope)
	}
	if strings.HasSuffix(scope, "urn:matrix:client:device:") {
		t.Error("expected a non-empty device ID in the scope")
	}
}

func TestMSC3861SSOCompat_RedirectActionRegisterSetsPromptCreate(t *testing.T) {
	t.Parallel()

	routers, _, closeDB := setupMSC3861SSORouters(t, config.MSC3861Config{
		Issuer:                "https://auth.example.com",
		ClientID:              "test-client-id",
		ClientSecret:          "test-client-secret",
		AuthorizationEndpoint: "https://auth.example.com/custom/auth",
		TokenEndpoint:         "https://auth.example.com/custom/token",
		SSOCallbackURL:        "https://matrix.example.com/_matrix/client/v3/login/sso/callback",
	})
	defer closeDB()

	req := test.NewRequest(t, http.MethodGet, "/_matrix/client/v3/login/sso/redirect", test.WithQueryParams(map[string]string{
		"redirectUrl": "element://vector/webapp",
		"action":      "register",
	}))
	rec := httptest.NewRecorder()
	routers.Client.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d: %s", rec.Code, rec.Body.String())
	}

	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("failed to parse Location: %v", err)
	}
	if got := u.Query().Get("prompt"); got != "create" {
		t.Errorf("expected prompt=create for action=register, got %q", got)
	}

	// action=login (or no action) must not set prompt.
	req = test.NewRequest(t, http.MethodGet, "/_matrix/client/v3/login/sso/redirect", test.WithQueryParams(map[string]string{
		"redirectUrl": "element://vector/webapp",
		"action":      "login",
	}))
	rec = httptest.NewRecorder()
	routers.Client.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d: %s", rec.Code, rec.Body.String())
	}
	u, err = url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("failed to parse Location: %v", err)
	}
	if got := u.Query().Get("prompt"); got != "" {
		t.Errorf("expected no prompt for action=login, got %q", got)
	}
}

func TestMSC3861SSOCompat_RedirectDiscoversEndpoints(t *testing.T) {
	t.Parallel()

	var discoveryHits int32
	mockProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			atomic.AddInt32(&discoveryHits, 1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"authorization_endpoint": "https://auth.example.com/discovery/auth",
				"token_endpoint":         "https://auth.example.com/discovery/token",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockProvider.Close()

	routers, _, closeDB := setupMSC3861SSORouters(t, config.MSC3861Config{
		Issuer:         mockProvider.URL,
		ClientID:       "test-client-id",
		ClientSecret:   "test-client-secret",
		SSOCallbackURL: "https://matrix.example.com/_matrix/client/v3/login/sso/callback",
	})
	defer closeDB()

	req := test.NewRequest(t, http.MethodGet, "/_matrix/client/v3/login/sso/redirect", test.WithQueryParams(map[string]string{
		"redirectUrl": "element://vector/webapp",
	}))
	rec := httptest.NewRecorder()
	routers.Client.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d: %s", rec.Code, rec.Body.String())
	}

	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "https://auth.example.com/discovery/auth?") {
		t.Fatalf("expected redirect to discovered authorization endpoint, got %s", location)
	}
	if atomic.LoadInt32(&discoveryHits) != 1 {
		t.Fatalf("expected OIDC discovery to be hit once, got %d", discoveryHits)
	}
}

func TestMSC3861SSOCompat_RedirectRequiresRedirectUrl(t *testing.T) {
	t.Parallel()
	routers, _, closeDB := setupMSC3861SSORouters(t, config.MSC3861Config{
		Issuer:       "https://auth.example.com",
		ClientID:     "test-client-id",
		ClientSecret: "test-client-secret",
	})
	defer closeDB()

	req := test.NewRequest(t, http.MethodGet, "/_matrix/client/v3/login/sso/redirect")
	rec := httptest.NewRecorder()
	routers.Client.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMSC3861SSOCompat_CallbackSuccess(t *testing.T) {
	t.Parallel()

	accessToken := "oidc-access-token-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	var tokenEndpointHits, introspectionHits int32

	mockProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			atomic.AddInt32(&tokenEndpointHits, 1)
			if err := r.ParseForm(); err != nil {
				t.Fatalf("failed to parse token form: %v", err)
			}
			if r.Form.Get("grant_type") != "authorization_code" {
				t.Errorf("expected grant_type authorization_code, got %s", r.Form.Get("grant_type"))
			}
			if r.Form.Get("code") != "mock-auth-code" {
				t.Errorf("unexpected code: %s", r.Form.Get("code"))
			}
			if r.Form.Get("code_verifier") == "" {
				t.Error("expected code_verifier")
			}
			if r.Form.Get("redirect_uri") != "https://matrix.example.com/_matrix/client/v3/login/sso/callback" {
				t.Errorf("unexpected redirect_uri: %s", r.Form.Get("redirect_uri"))
			}

			user, pass, ok := r.BasicAuth()
			if !ok || user != "test-client-id" || pass != "test-client-secret" {
				t.Errorf("expected basic auth test-client-id:test-client-secret, got ok=%v user=%s pass=%s", ok, user, pass)
			}

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": accessToken,
				"token_type":   "Bearer",
			})
		case "/introspect":
			atomic.AddInt32(&introspectionHits, 1)
			if err := r.ParseForm(); err != nil {
				t.Fatalf("failed to parse introspect form: %v", err)
			}
			if r.Form.Get("token") != accessToken {
				t.Errorf("expected token %s, got %s", accessToken, r.Form.Get("token"))
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"active":   true,
				"sub":      "alice-oidc-sub",
				"username": "alice",
				"scope":    "openid urn:matrix:client:api:*",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockProvider.Close()

	routers, cfg, closeDB := setupMSC3861SSORouters(t, config.MSC3861Config{
		Issuer:                mockProvider.URL,
		ClientID:              "test-client-id",
		ClientSecret:          "test-client-secret",
		AuthorizationEndpoint: mockProvider.URL + "/auth",
		TokenEndpoint:         mockProvider.URL + "/token",
		IntrospectionEndpoint: mockProvider.URL + "/introspect",
		SSOCallbackURL:        "https://matrix.example.com/_matrix/client/v3/login/sso/callback",
	})
	defer closeDB()

	// Start the SSO flow to obtain a valid state.
	redirectReq := test.NewRequest(t, http.MethodGet, "/_matrix/client/v3/login/sso/redirect", test.WithQueryParams(map[string]string{
		"redirectUrl": "element://vector/webapp?element-desktop-ssoid=abc",
	}))
	redirectRec := httptest.NewRecorder()
	routers.Client.ServeHTTP(redirectRec, redirectReq)
	if redirectRec.Code != http.StatusFound {
		t.Fatalf("expected redirect 302, got %d: %s", redirectRec.Code, redirectRec.Body.String())
	}
	location, err := url.Parse(redirectRec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("failed to parse redirect location: %v", err)
	}
	state := location.Query().Get("state")
	if state == "" {
		t.Fatal("expected state in redirect location")
	}

	// Invoke the callback as the OIDC provider would.
	callbackReq := test.NewRequest(t, http.MethodGet, "/_matrix/client/v3/login/sso/callback", test.WithQueryParams(map[string]string{
		"state": state,
		"code":  "mock-auth-code",
	}))
	callbackRec := httptest.NewRecorder()
	routers.Client.ServeHTTP(callbackRec, callbackReq)
	if callbackRec.Code != http.StatusFound {
		t.Fatalf("expected callback 302, got %d: %s", callbackRec.Code, callbackRec.Body.String())
	}

	callbackLocation := callbackRec.Header().Get("Location")
	if callbackLocation == "" {
		t.Fatal("expected callback Location header")
	}
	u, err := url.Parse(callbackLocation)
	if err != nil {
		t.Fatalf("failed to parse callback location: %v", err)
	}
	if !strings.HasPrefix(u.String(), "element://vector/webapp") {
		t.Fatalf("expected redirect to element webapp, got %s", callbackLocation)
	}
	loginToken := u.Query().Get("loginToken")
	if loginToken == "" {
		t.Fatalf("expected loginToken in callback location, got %s", callbackLocation)
	}

	if atomic.LoadInt32(&tokenEndpointHits) != 1 {
		t.Fatalf("expected token endpoint hit once, got %d", tokenEndpointHits)
	}
	if atomic.LoadInt32(&introspectionHits) != 1 {
		t.Fatalf("expected introspection endpoint hit once, got %d", introspectionHits)
	}

	// The returned login token must be redeemable for a Matrix session.
	loginReq := test.NewRequest(t, http.MethodPost, "/_matrix/client/v3/login", test.WithJSONBody(t, map[string]any{
		"type":  authtypes.LoginTypeToken,
		"token": loginToken,
	}))
	loginRec := httptest.NewRecorder()
	routers.Client.ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("expected 200 redeeming login token, got %d: %s", loginRec.Code, loginRec.Body.String())
	}
	var loginResp loginResponse
	if err := json.Unmarshal(loginRec.Body.Bytes(), &loginResp); err != nil {
		t.Fatalf("failed to parse login response: %v", err)
	}
	expectedUserID := "@alice:" + string(cfg.Global.ServerName)
	if loginResp.UserID != expectedUserID {
		t.Errorf("expected user_id %s, got %s", expectedUserID, loginResp.UserID)
	}
	if loginResp.AccessToken == "" {
		t.Error("expected access_token")
	}
}

func TestMSC3861SSOCompat_CallbackInvalidState(t *testing.T) {
	t.Parallel()
	routers, _, closeDB := setupMSC3861SSORouters(t, config.MSC3861Config{
		Issuer:       "https://auth.example.com",
		ClientID:     "test-client-id",
		ClientSecret: "test-client-secret",
	})
	defer closeDB()

	req := test.NewRequest(t, http.MethodGet, "/_matrix/client/v3/login/sso/callback", test.WithQueryParams(map[string]string{
		"state": "not-a-valid-state",
		"code":  "mock-auth-code",
	}))
	rec := httptest.NewRecorder()
	routers.Client.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMSC3861SSOCompat_CallbackProviderError(t *testing.T) {
	t.Parallel()
	routers, _, closeDB := setupMSC3861SSORouters(t, config.MSC3861Config{
		Issuer:       "https://auth.example.com",
		ClientID:     "test-client-id",
		ClientSecret: "test-client-secret",
	})
	defer closeDB()

	req := test.NewRequest(t, http.MethodGet, "/_matrix/client/v3/login/sso/callback", test.WithQueryParams(map[string]string{
		"error":             "access_denied",
		"error_description": "user denied",
	}))
	rec := httptest.NewRecorder()
	routers.Client.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMSC3861SSOCompat_CallbackMissingCode(t *testing.T) {
	t.Parallel()
	routers, _, closeDB := setupMSC3861SSORouters(t, config.MSC3861Config{
		Issuer:       "https://auth.example.com",
		ClientID:     "test-client-id",
		ClientSecret: "test-client-secret",
	})
	defer closeDB()

	req := test.NewRequest(t, http.MethodGet, "/_matrix/client/v3/login/sso/callback", test.WithQueryParams(map[string]string{
		"state": "anything",
	}))
	rec := httptest.NewRecorder()
	routers.Client.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMSC3861SSOCompat_CallbackTokenExchangeFailure(t *testing.T) {
	t.Parallel()

	mockProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": "invalid_client",
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockProvider.Close()

	routers, _, closeDB := setupMSC3861SSORouters(t, config.MSC3861Config{
		Issuer:                mockProvider.URL,
		ClientID:              "test-client-id",
		ClientSecret:          "test-client-secret",
		AuthorizationEndpoint: mockProvider.URL + "/auth",
		TokenEndpoint:         mockProvider.URL + "/token",
		SSOCallbackURL:        "https://matrix.example.com/_matrix/client/v3/login/sso/callback",
	})
	defer closeDB()

	// Obtain a valid state.
	redirectReq := test.NewRequest(t, http.MethodGet, "/_matrix/client/v3/login/sso/redirect", test.WithQueryParams(map[string]string{
		"redirectUrl": "element://vector/webapp",
	}))
	redirectRec := httptest.NewRecorder()
	routers.Client.ServeHTTP(redirectRec, redirectReq)
	location, _ := url.Parse(redirectRec.Header().Get("Location"))
	state := location.Query().Get("state")

	req := test.NewRequest(t, http.MethodGet, "/_matrix/client/v3/login/sso/callback", test.WithQueryParams(map[string]string{
		"state": state,
		"code":  "bad-code",
	}))
	rec := httptest.NewRecorder()
	routers.Client.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMSC3861SSOCompat_CallbackInactiveToken(t *testing.T) {
	t.Parallel()

	accessToken := "inactive-token-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	mockProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": accessToken,
				"token_type":   "Bearer",
			})
		case "/introspect":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"active": false,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockProvider.Close()

	routers, _, closeDB := setupMSC3861SSORouters(t, config.MSC3861Config{
		Issuer:                mockProvider.URL,
		ClientID:              "test-client-id",
		ClientSecret:          "test-client-secret",
		AuthorizationEndpoint: mockProvider.URL + "/auth",
		TokenEndpoint:         mockProvider.URL + "/token",
		IntrospectionEndpoint: mockProvider.URL + "/introspect",
		SSOCallbackURL:        "https://matrix.example.com/_matrix/client/v3/login/sso/callback",
	})
	defer closeDB()

	redirectReq := test.NewRequest(t, http.MethodGet, "/_matrix/client/v3/login/sso/redirect", test.WithQueryParams(map[string]string{
		"redirectUrl": "element://vector/webapp",
	}))
	redirectRec := httptest.NewRecorder()
	routers.Client.ServeHTTP(redirectRec, redirectReq)
	location, _ := url.Parse(redirectRec.Header().Get("Location"))
	state := location.Query().Get("state")

	req := test.NewRequest(t, http.MethodGet, "/_matrix/client/v3/login/sso/callback", test.WithQueryParams(map[string]string{
		"state": state,
		"code":  "mock-code",
	}))
	rec := httptest.NewRecorder()
	routers.Client.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for inactive token, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "M_UNKNOWN_TOKEN") {
		t.Fatalf("expected M_UNKNOWN_TOKEN in response body, got %s", body)
	}
}

func TestMSC3861SSOCompat_CallbackClientSecretPost(t *testing.T) {
	t.Parallel()

	accessToken := "post-token-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	var receivedClientSecret string
	mockProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("failed to parse form: %v", err)
		}
		receivedClientSecret = r.Form.Get("client_secret")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": accessToken,
			"token_type":   "Bearer",
		})
	}))
	defer mockProvider.Close()

	mockIntrospect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"active":   true,
			"sub":      "bob",
			"username": "bob",
		})
	}))
	defer mockIntrospect.Close()

	routers, _, closeDB := setupMSC3861SSORouters(t, config.MSC3861Config{
		Issuer:                mockProvider.URL,
		ClientID:              "test-client-id",
		ClientSecret:          "test-client-secret",
		ClientAuthMethod:      "client_secret_post",
		AuthorizationEndpoint: mockProvider.URL + "/auth",
		TokenEndpoint:         mockProvider.URL + "/token",
		IntrospectionEndpoint: mockIntrospect.URL + "/introspect",
		SSOCallbackURL:        "https://matrix.example.com/_matrix/client/v3/login/sso/callback",
	})
	defer closeDB()

	redirectReq := test.NewRequest(t, http.MethodGet, "/_matrix/client/v3/login/sso/redirect", test.WithQueryParams(map[string]string{
		"redirectUrl": "element://vector/webapp",
	}))
	redirectRec := httptest.NewRecorder()
	routers.Client.ServeHTTP(redirectRec, redirectReq)
	location, _ := url.Parse(redirectRec.Header().Get("Location"))
	state := location.Query().Get("state")

	req := test.NewRequest(t, http.MethodGet, "/_matrix/client/v3/login/sso/callback", test.WithQueryParams(map[string]string{
		"state": state,
		"code":  "mock-code",
	}))
	rec := httptest.NewRecorder()
	routers.Client.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d: %s", rec.Code, rec.Body.String())
	}
	if receivedClientSecret != "test-client-secret" {
		t.Errorf("expected client_secret in token request body, got %q", receivedClientSecret)
	}
}

func TestMSC3861SSOCompat_CallbackReusesExistingUser(t *testing.T) {
	t.Parallel()

	accessToken := "reuses-token-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	mockProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": accessToken,
				"token_type":   "Bearer",
			})
		case "/introspect":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"active":   true,
				"sub":      "existing-user-sub",
				"username": "existinguser",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockProvider.Close()

	routers, cfg, closeDB := setupMSC3861SSORouters(t, config.MSC3861Config{
		Issuer:                mockProvider.URL,
		ClientID:              "test-client-id",
		ClientSecret:          "test-client-secret",
		AuthorizationEndpoint: mockProvider.URL + "/auth",
		TokenEndpoint:         mockProvider.URL + "/token",
		IntrospectionEndpoint: mockProvider.URL + "/introspect",
		SSOCallbackURL:        "https://matrix.example.com/_matrix/client/v3/login/sso/callback",
	})
	defer closeDB()

	// Pre-create the user account.
	expectedUserID := "@existinguser:" + string(cfg.Global.ServerName)
	// We can't directly create an account here easily, but the first SSO login
	// will auto-provision it. We just verify the callback succeeds and the
	// resulting login token is for the expected user.
	redirectReq := test.NewRequest(t, http.MethodGet, "/_matrix/client/v3/login/sso/redirect", test.WithQueryParams(map[string]string{
		"redirectUrl": "element://vector/webapp",
	}))
	redirectRec := httptest.NewRecorder()
	routers.Client.ServeHTTP(redirectRec, redirectReq)
	location, _ := url.Parse(redirectRec.Header().Get("Location"))
	state := location.Query().Get("state")

	req := test.NewRequest(t, http.MethodGet, "/_matrix/client/v3/login/sso/callback", test.WithQueryParams(map[string]string{
		"state": state,
		"code":  "mock-code",
	}))
	rec := httptest.NewRecorder()
	routers.Client.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d: %s", rec.Code, rec.Body.String())
	}

	u, _ := url.Parse(rec.Header().Get("Location"))
	loginToken := u.Query().Get("loginToken")

	loginReq := test.NewRequest(t, http.MethodPost, "/_matrix/client/v3/login", test.WithJSONBody(t, map[string]any{
		"type":  authtypes.LoginTypeToken,
		"token": loginToken,
	}))
	loginRec := httptest.NewRecorder()
	routers.Client.ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", loginRec.Code, loginRec.Body.String())
	}
	var loginResp loginResponse
	if err := json.Unmarshal(loginRec.Body.Bytes(), &loginResp); err != nil {
		t.Fatalf("failed to parse login response: %v", err)
	}
	if loginResp.UserID != expectedUserID {
		t.Errorf("expected user_id %s, got %s", expectedUserID, loginResp.UserID)
	}
}

func TestGeneratePKCEVerifier(t *testing.T) {
	t.Parallel()
	v, err := generatePKCEVerifier()
	if err != nil {
		t.Fatalf("generatePKCEVerifier failed: %v", err)
	}
	if len(v) != 43 {
		t.Errorf("expected verifier length 43, got %d", len(v))
	}
	if strings.Contains(v, "+") || strings.Contains(v, "/") || strings.Contains(v, "=") {
		t.Errorf("verifier is not URL-safe: %s", v)
	}
}

func TestPKCEChallenge(t *testing.T) {
	t.Parallel()
	verifier := "test-verifier"
	ch1 := pkceChallenge(verifier)
	ch2 := pkceChallenge(verifier)
	if ch1 != ch2 {
		t.Errorf("PKCE challenge not deterministic: %s vs %s", ch1, ch2)
	}
	if ch1 == "" {
		t.Error("expected non-empty challenge")
	}
	if strings.Contains(ch1, "+") || strings.Contains(ch1, "/") || strings.Contains(ch1, "=") {
		t.Errorf("challenge is not URL-safe: %s", ch1)
	}
}

func TestRequestScheme(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		req      *http.Request
		expected string
	}{
		{
			name: "forwarded proto https",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "http://localhost/foo", nil)
				r.Header.Set("X-Forwarded-Proto", "https")
				return r
			}(),
			expected: "https",
		},
		{
			name: "TLS request",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "https://localhost/foo", nil)
				return r
			}(),
			expected: "https",
		},
		{
			name: "plain HTTP",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "http://localhost/foo", nil)
				return r
			}(),
			expected: "http",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := requestScheme(tt.req); got != tt.expected {
				t.Errorf("requestScheme() = %s, want %s", got, tt.expected)
			}
		})
	}
}

func TestSSOCompatHandlerCallbackURL(t *testing.T) {
	t.Parallel()
	h := newSSOCompatHandler(&config.MSC3861Config{
		SSOCallbackURL: "https://custom.example.com/callback",
	}, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "http://localhost/_matrix/client/v3/login/sso/redirect", nil)
	if got := h.callbackURL(req); got != "https://custom.example.com/callback" {
		t.Errorf("expected custom callback URL, got %s", got)
	}

	h2 := newSSOCompatHandler(&config.MSC3861Config{}, nil, nil, nil, nil)
	req2 := httptest.NewRequest(http.MethodGet, "http://example.com/_matrix/client/v3/login/sso/redirect", nil)
	req2.Header.Set("X-Forwarded-Proto", "https")
	if got := h2.callbackURL(req2); got != "https://example.com/_matrix/client/v3/login/sso/callback" {
		t.Errorf("expected derived callback URL, got %s", got)
	}
}
