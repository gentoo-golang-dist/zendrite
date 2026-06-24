// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only.

package routing

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"

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
)

// setupMSC3861Routers creates a full router stack with MSC3861 enabled for testing.
// These tests only verify HTTP routing behavior (status codes, response bodies),
// not database semantics, so we always use SQLite to avoid the shared-postgres
// schema race that occurs when multiple parallel subtests DROP SCHEMA on the
// same CI database.
func setupMSC3861Routers(t *testing.T) (httputil.Routers, *config.Zendrite, func()) {
	t.Helper()
	cfg, processCtx, closeDB := testrig.CreateConfig(t, test.DBTypeSQLite)
	cfg.ClientAPI.RateLimiting.Enabled = false
	cfg.MSCs.MSCs = []string{"msc3861"}
	cfg.MSCs.MSC3861 = config.MSC3861Config{
		Issuer:               "https://auth.example.com",
		ClientID:             "test-client-id",
		ClientSecret:         "test-client-secret",
		AdminToken:           "test-admin-token",
		AccountManagementURL: "https://auth.example.com/account",
	}
	// WellKnownClientName needed for well-known endpoint registration
	cfg.Global.WellKnownClientName = "https://matrix.example.com"

	// Register a dummy application service so we can verify that
	// m.login.application_service registrations remain available.
	asRegex := regexp.MustCompile(`@_appservice_.*:test`)
	cfg.ClientAPI.Derived.ApplicationServices = []config.ApplicationService{
		{
			ID:              "fake-as",
			ASToken:         "fake-as-token",
			HSToken:         "fake-hs-token",
			URL:             "null",
			SenderLocalpart: "_appservice_bot",
			NamespaceMap: map[string][]config.ApplicationServiceNamespace{
				"users": {
					{
						Exclusive:    true,
						Regex:        asRegex.String(),
						RegexpObject: asRegex,
					},
				},
			},
		},
	}

	natsInstance := jetstream.NATSInstance{}
	cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
	caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
	rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
	rsAPI.SetFederationAPI(nil, nil)
	userAPI := userapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, rsAPI, nil, caching.DisableMetrics, testIsBlacklistedOrBackingOff)

	routers := httputil.NewRouters()
	Setup(routers, cfg, nil, nil, userAPI, nil, nil, nil, nil, nil, nil, nil, nil, caching.DisableMetrics)

	return routers, cfg, closeDB
}

func TestMSC3861_DisabledEndpoints(t *testing.T) {
	t.Parallel()

	type endpointTest struct {
		name   string
		method string
		path   string
	}

	endpoints := []endpointTest{
		{"register", http.MethodPost, "/_matrix/client/v3/register"},
		{"register available", http.MethodGet, "/_matrix/client/v3/register/available"},
		{"password", http.MethodPost, "/_matrix/client/v3/account/password"},
		{"deactivate", http.MethodPost, "/_matrix/client/v3/account/deactivate"},
		{"3pid POST", http.MethodPost, "/_matrix/client/v3/account/3pid"},
		{"3pid delete", http.MethodPost, "/_matrix/client/v3/account/3pid/delete"},
		{"3pid email requestToken", http.MethodPost, "/_matrix/client/v3/account/3pid/email/requestToken"},
		{"register email requestToken", http.MethodPost, "/_matrix/client/v3/register/email/requestToken"},
		{"auth fallback web", http.MethodGet, "/_matrix/client/v3/auth/m.login.sso/fallback/web"},
		{"device PUT", http.MethodPut, "/_matrix/client/v3/devices/ABCDEF"},
		{"device DELETE", http.MethodDelete, "/_matrix/client/v3/devices/ABCDEF"},
		{"delete_devices", http.MethodPost, "/_matrix/client/v3/delete_devices"},
		{"logout", http.MethodPost, "/_matrix/client/v3/logout"},
		{"logout all", http.MethodPost, "/_matrix/client/v3/logout/all"},
		{"login POST", http.MethodPost, "/_matrix/client/v3/login"},
	}

	routers, _, closeDB := setupMSC3861Routers(t)
	defer closeDB()

	for _, ep := range endpoints {
		t.Run(ep.name, func(t *testing.T) {
			req := test.NewRequest(t, ep.method, ep.path)
			rec := httptest.NewRecorder()
			routers.Client.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s: expected 403, got %d: %s", ep.method, ep.path, rec.Code, rec.Body.String())
			}

			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("failed to parse response: %v", err)
			}
			if errcode, ok := body["errcode"].(string); !ok || errcode != string(spec.ErrorForbidden) {
				t.Errorf("expected errcode %s, got %v", spec.ErrorForbidden, body["errcode"])
			}
		})
	}
}

func TestMSC3861_LoginFlowsGET(t *testing.T) {
	t.Parallel()
	routers, _, closeDB := setupMSC3861Routers(t)
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
		t.Fatalf("expected 2 flows (sso, token), got %d: %+v", len(resp.Flows), resp.Flows)
	}
	gotTypes := make(map[string]bool)
	for _, f := range resp.Flows {
		gotTypes[f.Type] = true
	}
	if !gotTypes[authtypes.LoginTypeSSO] {
		t.Errorf("expected %s flow to be advertised", authtypes.LoginTypeSSO)
	}
	if !gotTypes[authtypes.LoginTypeToken] {
		t.Errorf("expected %s flow to be advertised", authtypes.LoginTypeToken)
	}
}

func TestMSC3861_LoginPOST(t *testing.T) {
	t.Parallel()
	routers, _, closeDB := setupMSC3861Routers(t)
	defer closeDB()

	req := test.NewRequest(t, http.MethodPost, "/_matrix/client/v3/login")
	rec := httptest.NewRecorder()
	routers.Client.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if errcode, ok := body["errcode"].(string); !ok || errcode != string(spec.ErrorForbidden) {
		t.Errorf("expected errcode %s, got %v", spec.ErrorForbidden, body["errcode"])
	}
}

func TestMSC3861_WellKnown(t *testing.T) {
	t.Parallel()
	routers, cfg, closeDB := setupMSC3861Routers(t)
	defer closeDB()

	req := test.NewRequest(t, http.MethodGet, "/.well-known/matrix/client")
	rec := httptest.NewRecorder()
	routers.WellKnown.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp WellKnownClientResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.Authentication == nil {
		t.Fatal("expected m.authentication in well-known response")
	}
	if resp.Authentication.Issuer != cfg.MSCs.MSC3861.Issuer {
		t.Errorf("expected issuer %s, got %s", cfg.MSCs.MSC3861.Issuer, resp.Authentication.Issuer)
	}
	if resp.Authentication.Account != cfg.MSCs.MSC3861.AccountManagementURL {
		t.Errorf("expected account %s, got %s", cfg.MSCs.MSC3861.AccountManagementURL, resp.Authentication.Account)
	}
}

func TestMSC3861_AuthMetadata(t *testing.T) {
	t.Parallel()
	routers, cfg, closeDB := setupMSC3861Routers(t)
	defer closeDB()

	req := test.NewRequest(t, http.MethodGet, "/_matrix/client/v1/auth_metadata")
	rec := httptest.NewRecorder()
	routers.Client.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp AuthMetadataResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.Issuer != cfg.MSCs.MSC3861.Issuer {
		t.Errorf("expected issuer %s, got %s", cfg.MSCs.MSC3861.Issuer, resp.Issuer)
	}
	if resp.AuthorizationEndpoint == "" {
		t.Error("expected non-empty authorization_endpoint")
	}
	if resp.TokenEndpoint == "" {
		t.Error("expected non-empty token_endpoint")
	}
	if resp.RegistrationEndpoint == "" {
		t.Error("expected non-empty registration_endpoint")
	}
	if resp.RevocationEndpoint == "" {
		t.Error("expected non-empty revocation_endpoint")
	}
	if resp.JWKSURI == "" {
		t.Error("expected non-empty jwks_uri")
	}
}

func TestMSC3861_MASAdminRoutesRequireAdminToken(t *testing.T) {
	t.Parallel()

	cfg, processCtx, closeDB := testrig.CreateConfig(t, test.DBTypeSQLite)
	defer closeDB()
	cfg.ClientAPI.RateLimiting.Enabled = false
	cfg.MSCs.MSCs = []string{"msc3861"}
	cfg.MSCs.MSC3861 = config.MSC3861Config{
		Issuer:       "https://auth.example.com",
		ClientID:     "test-client-id",
		ClientSecret: "test-client-secret",
		// AdminToken deliberately left empty.
	}
	cfg.Global.WellKnownClientName = "https://matrix.example.com"

	natsInstance := jetstream.NATSInstance{}
	cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
	caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
	rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
	rsAPI.SetFederationAPI(nil, nil)
	userAPI := userapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, rsAPI, nil, caching.DisableMetrics, testIsBlacklistedOrBackingOff)

	routers := httputil.NewRouters()
	Setup(routers, cfg, nil, nil, userAPI, nil, nil, nil, nil, nil, nil, nil, nil, caching.DisableMetrics)

	req := test.NewRequest(t, http.MethodGet, "/_synapse/admin/v1/username_available?username=alice")
	rec := httptest.NewRecorder()
	routers.SynapseAdmin.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 when admin_token is empty, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMSC3861_DevicesGETStillWorks(t *testing.T) {
	t.Parallel()
	routers, _, closeDB := setupMSC3861Routers(t)
	defer closeDB()

	// GET /devices should NOT be blocked by MSC3861 (only PUT/DELETE are blocked).
	// Without auth it returns 401, not 403 — that proves the endpoint is still
	// routed to the real handler rather than the forbidden handler.
	req := test.NewRequest(t, http.MethodGet, "/_matrix/client/v3/devices")
	rec := httptest.NewRecorder()
	routers.Client.ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Error("GET /devices should not be forbidden when MSC3861 is enabled")
	}
}

func TestMSC3861_ApplicationServiceRegisterStillWorks(t *testing.T) {
	t.Parallel()
	routers, _, closeDB := setupMSC3861Routers(t)
	defer closeDB()

	body, err := json.Marshal(map[string]any{
		"type":     "m.login.application_service",
		"username": "_appservice_alice",
	})
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	req := test.NewRequest(t, http.MethodPost, "/_matrix/client/v3/register?access_token=fake-as-token")
	req.Body = io.NopCloser(bytes.NewReader(body))
	rec := httptest.NewRecorder()
	routers.Client.ServeHTTP(rec, req)

	// A 200/401 response means the request reached the real registration handler
	// and was NOT rejected by the MSC3861 forbidden handler.
	if rec.Code == http.StatusForbidden {
		t.Fatalf("application service registration should not be forbidden under MSC3861: %s", rec.Body.String())
	}
	if rec.Code != http.StatusOK && rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 200 or 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMSC3861_VersionsAdvertisesMSC2965(t *testing.T) {
	t.Parallel()
	routers, _, closeDB := setupMSC3861Routers(t)
	defer closeDB()

	req := test.NewRequest(t, http.MethodGet, "/_matrix/client/versions")
	rec := httptest.NewRecorder()
	routers.Client.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		UnstableFeatures map[string]bool `json:"unstable_features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if !resp.UnstableFeatures["org.matrix.msc2965"] {
		t.Error("expected org.matrix.msc2965 unstable feature to be advertised")
	}
	if !resp.UnstableFeatures["org.matrix.msc2967"] {
		t.Error("expected org.matrix.msc2967 unstable feature to be advertised")
	}
}

func TestMSC3861_AuthMetadataUnstable(t *testing.T) {
	t.Parallel()
	routers, cfg, closeDB := setupMSC3861Routers(t)
	defer closeDB()

	req := test.NewRequest(t, http.MethodGet, "/_matrix/client/unstable/org.matrix.msc2965/auth_metadata")
	rec := httptest.NewRecorder()
	routers.Client.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp AuthMetadataResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.Issuer != cfg.MSCs.MSC3861.Issuer {
		t.Errorf("expected issuer %s, got %s", cfg.MSCs.MSC3861.Issuer, resp.Issuer)
	}
}

func TestMSC3861_AuthMetadataDisabled(t *testing.T) {
	t.Parallel()

	cfg, processCtx, closeDB := testrig.CreateConfig(t, test.DBTypeSQLite)
	defer closeDB()
	cfg.ClientAPI.RateLimiting.Enabled = false
	// MSC3861 intentionally not enabled.
	cfg.Global.WellKnownClientName = "https://matrix.example.com"

	natsInstance := jetstream.NATSInstance{}
	cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
	caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
	rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
	rsAPI.SetFederationAPI(nil, nil)
	userAPI := userapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, rsAPI, nil, caching.DisableMetrics, testIsBlacklistedOrBackingOff)

	routers := httputil.NewRouters()
	Setup(routers, cfg, nil, nil, userAPI, nil, nil, nil, nil, nil, nil, nil, nil, caching.DisableMetrics)

	req := test.NewRequest(t, http.MethodGet, "/_matrix/client/v1/auth_metadata")
	rec := httptest.NewRecorder()
	routers.Client.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when MSC3861 is disabled, got %d: %s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if errcode, ok := body["errcode"].(string); !ok || errcode != string(spec.ErrorUnrecognized) {
		t.Errorf("expected errcode %s, got %v", spec.ErrorUnrecognized, body["errcode"])
	}
}
