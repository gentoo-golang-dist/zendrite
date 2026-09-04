// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only.

package routing

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

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

// These tests run the /_synapse/mas provisioning API end to end over real
// HTTP: the full router stack is served by httptest and driven by masClient,
// a client that mirrors MAS's own HTTP behavior (bearer auth, JSON bodies and
// errcode-based error handling, as implemented in matrix-synapse's modern.rs).
// The introspection endpoint is a programmable fake MAS, so the flows cover
// the interaction between provisioning and the MSC3861 token binding path.

// masClient drives the provisioning endpoints the way MAS does.
type masClient struct {
	t     *testing.T
	base  string
	token string
	http  *http.Client
}

// masAPIError is the error shape every endpoint returns on failure.
type masAPIError struct {
	ErrCode string `json:"errcode"`
	Error   string `json:"error"`
}

// do performs a request like MAS would: bearer token, JSON body where given.
// It returns the status code, the response body and the parsed errcode (empty
// on 2xx), mirroring MAS's error_for_synapse_error behavior.
func (c *masClient) do(method, path string, body any) (int, []byte, string) {
	c.t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			c.t.Fatalf("failed to marshal request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		c.t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("request failed: %v", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		c.t.Fatalf("failed to read response: %v", err)
	}
	errcode := ""
	if res.StatusCode >= 300 {
		var apiErr masAPIError
		if json.Unmarshal(raw, &apiErr) == nil {
			errcode = apiErr.ErrCode
		}
	}
	return res.StatusCode, raw, errcode
}

// provisionUser posts the provision_user body; MAS distinguishes 201 (created)
// from 200 (existed) exactly like this.
func (c *masClient) provisionUser(body map[string]any) int {
	c.t.Helper()
	code, raw, errcode := c.do(http.MethodPost, "/_synapse/mas/provision_user", body)
	if code != http.StatusCreated && code != http.StatusOK {
		c.t.Fatalf("provision_user failed: %d %s (%s)", code, raw, errcode)
	}
	return code
}

func (c *masClient) queryUser(localpart string) (int, map[string]any, string) {
	c.t.Helper()
	code, raw, errcode := c.do(http.MethodGet, "/_synapse/mas/query_user?localpart="+localpart, nil)
	var body map[string]any
	if code == http.StatusOK {
		if err := json.Unmarshal(raw, &body); err != nil {
			c.t.Fatalf("failed to parse query_user response: %v", err)
		}
	}
	return code, body, errcode
}

// fakeMASIntrospection is a programmable OIDC introspection endpoint. It
// answers for the tokens it was programmed with and reports active:false for
// everything else.
type fakeMASIntrospection struct {
	mu     sync.Mutex
	tokens map[string]map[string]any
}

func (f *fakeMASIntrospection) set(token string, response map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[token] = response
}

func (f *fakeMASIntrospection) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	response, ok := f.tokens[r.PostForm.Get("token")]
	if !ok {
		response = map[string]any{"active": false}
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

// setupMASIntegrationRig builds the full stack for a database backend: an
// httptest server serving the SynapseAdmin router (/_synapse), a fake MAS
// introspection endpoint and the user API for direct state setup/assertions.
func setupMASIntegrationRig(t *testing.T, dbType test.DBType) (*masClient, userapiAPI.ClientUserAPI, *fakeMASIntrospection, *config.Zendrite, func()) {
	t.Helper()

	introspection := &fakeMASIntrospection{tokens: map[string]map[string]any{}}
	introspectionServer := httptest.NewServer(introspection)
	t.Cleanup(introspectionServer.Close)

	cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
	cfg.ClientAPI.RateLimiting.Enabled = false
	cfg.MSCs.MSCs = []string{"msc3861"}
	cfg.MSCs.MSC3861 = config.MSC3861Config{
		Issuer:                introspectionServer.URL,
		ClientID:              "test-client-id",
		ClientSecret:          "test-client-secret",
		IntrospectionEndpoint: introspectionServer.URL + "/introspect",
		AdminToken:            "test-admin-token",
	}
	cfg.UserAPI.MSCs = &cfg.MSCs

	natsInstance := jetstream.NATSInstance{}
	cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
	caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
	rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
	rsAPI.SetFederationAPI(nil, nil)
	userAPI := userapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, rsAPI, nil, caching.DisableMetrics, testIsBlacklistedOrBackingOff)

	routers := httputil.NewRouters()
	Setup(routers, cfg, nil, nil, userAPI, nil, nil, nil, nil, nil, nil, nil, nil, caching.DisableMetrics)

	srv := httptest.NewServer(routers.SynapseAdmin)
	t.Cleanup(srv.Close)

	cleanup := func() {
		srv.Close()
		closeDB()
	}
	client := &masClient{t: t, base: srv.URL, token: "test-admin-token", http: srv.Client()}
	return client, userAPI, introspection, cfg, cleanup
}

// introspectionForDevice builds the introspection response MAS would produce
// for an access token belonging to localpart with the given device scope.
func introspectionForDevice(localpart, deviceID string) map[string]any {
	scope := fmt.Sprintf("openid urn:matrix:client:device:%s", deviceID)
	return map[string]any{
		"active":   true,
		"sub":      "opaque-sub-" + localpart,
		"username": localpart,
		"scope":    scope,
	}
}

// TestMASIntegration_LoginProvisionFlow replays the sequence MAS performs
// when a brand new user logs in through the compatibility layer: check the
// localpart, provision the account, provision the device, then query the
// user. This is the flow that failed with 404s before the /_synapse/mas API
// existed.
func TestMASIntegration_LoginProvisionFlow(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		client, userAPI, _, cfg, cleanup := setupMASIntegrationRig(t, dbType)
		defer cleanup()

		const localpart = "newuser"
		userID := fmt.Sprintf("@%s:%s", localpart, cfg.ClientAPI.Matrix.ServerName)

		// 1. MAS checks the localpart is free.
		code, _, errcode := client.do(http.MethodGet, "/_synapse/mas/is_localpart_available?localpart="+localpart, nil)
		if code != http.StatusOK {
			t.Fatalf("is_localpart_available: expected 200, got %d (%s)", code, errcode)
		}

		// 2. Provision the account: first call creates it.
		if code := client.provisionUser(map[string]any{"localpart": localpart, "set_displayname": "New User"}); code != http.StatusCreated {
			t.Fatalf("provision_user: expected 201 on first call, got %d", code)
		}

		// 3. A second provisioning reports the account as existing.
		if code := client.provisionUser(map[string]any{"localpart": localpart}); code != http.StatusOK {
			t.Fatalf("provision_user: expected 200 on second call, got %d", code)
		}

		// 4. Provision the login device.
		code, _, errcode = client.do(http.MethodPost, "/_synapse/mas/upsert_device", map[string]any{
			"localpart":    localpart,
			"device_id":    "MASLOGIN1",
			"display_name": "Element Web",
		})
		if code != http.StatusCreated {
			t.Fatalf("upsert_device: expected 201, got %d (%s)", code, errcode)
		}

		// 5. The account and profile are now visible to MAS.
		code, body, errcode := client.queryUser(localpart)
		if code != http.StatusOK {
			t.Fatalf("query_user: expected 200, got %d (%s)", code, errcode)
		}
		if body["user_id"] != userID {
			t.Errorf("query_user user_id = %v, want %q", body["user_id"], userID)
		}
		if name, _ := body["display_name"].(string); name != "New User" {
			t.Errorf("query_user display_name = %v, want 'New User'", body["display_name"])
		}
		if deactivated, _ := body["is_deactivated"].(bool); deactivated {
			t.Error("query_user is_deactivated must be false")
		}

		// 6. An unknown user is a 404 with M_NOT_FOUND, which MAS relies on
		// to detect missing accounts.
		code, _, errcode = client.queryUser("neverregistered")
		if code != http.StatusNotFound || errcode != "M_NOT_FOUND" {
			t.Errorf("query_user unknown: expected 404 M_NOT_FOUND, got %d %q", code, errcode)
		}

		// The account is usable through the regular user API.
		var devices userapiAPI.QueryDevicesResponse
		if err := userAPI.QueryDevices(t.Context(), &userapiAPI.QueryDevicesRequest{UserID: userID}, &devices); err != nil {
			t.Fatalf("QueryDevices failed: %v", err)
		}
		if len(devices.Devices) != 1 || devices.Devices[0].ID != "MASLOGIN1" {
			t.Errorf("expected one device MASLOGIN1, got %+v", devices.Devices)
		}
	})
}

// TestMASIntegration_DeviceTokenSurvivesUpsert verifies the ordering hazard
// between provisioning and token binding: MAS provisions the device row
// before the client presents its access token, and the introspection path
// then binds the real token into that row. A later relogin upsert must never
// recreate the row, which would rotate the session ID and break the client.
func TestMASIntegration_DeviceTokenSurvivesUpsert(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		client, userAPI, introspection, _, cleanup := setupMASIntegrationRig(t, dbType)
		defer cleanup()

		const localpart = "relogin"

		client.provisionUser(map[string]any{"localpart": localpart})
		code, _, errcode := client.do(http.MethodPost, "/_synapse/mas/upsert_device", map[string]any{
			"localpart": localpart,
			"device_id": "REL1",
		})
		if code != http.StatusCreated {
			t.Fatalf("upsert_device: expected 201, got %d (%s)", code, errcode)
		}

		// The client's first authenticated request binds the real token.
		const realToken = "syz_real_access_token_1"
		introspection.set(realToken, introspectionForDevice(localpart, "REL1"))

		queryToken := func() (int64, string) {
			t.Helper()
			var res userapiAPI.QueryAccessTokenResponse
			if err := userAPI.QueryAccessToken(t.Context(), &userapiAPI.QueryAccessTokenRequest{AccessToken: realToken}, &res); err != nil {
				t.Fatalf("QueryAccessToken failed: %v", err)
			}
			if res.Device == nil {
				t.Fatal("expected device for bound token")
			}
			if res.Device.ID != "REL1" {
				t.Fatalf("token bound to device %q, want REL1", res.Device.ID)
			}
			return res.Device.SessionID, res.Device.DisplayName
		}

		sessionID, displayName := queryToken()

		// MAS upserts the same device again on relogin: the row must be
		// reused, not recreated.
		code, _, errcode = client.do(http.MethodPost, "/_synapse/mas/upsert_device", map[string]any{
			"localpart": localpart,
			"device_id": "REL1",
		})
		if code != http.StatusOK {
			t.Fatalf("relogin upsert_device: expected 200, got %d (%s)", code, errcode)
		}

		sessionID2, displayName2 := queryToken()
		if sessionID2 != sessionID {
			t.Errorf("session ID rotated across upsert: %d -> %d (device row was recreated)", sessionID, sessionID2)
		}
		if displayName2 != displayName {
			t.Errorf("display name changed across upsert: %q -> %q", displayName, displayName2)
		}
	})
}

// TestMASIntegration_SyncDevicesKeepsDehydratedDevice verifies that
// sync_devices reconciles the device list with MAS's view but never deletes
// the dehydrated device (MSC3814), which has no MAS session and therefore
// never appears in the target set.
func TestMASIntegration_SyncDevicesKeepsDehydratedDevice(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		client, userAPI, _, cfg, cleanup := setupMASIntegrationRig(t, dbType)
		defer cleanup()

		const localpart = "syncuser"
		userID := fmt.Sprintf("@%s:%s", localpart, cfg.ClientAPI.Matrix.ServerName)

		client.provisionUser(map[string]any{"localpart": localpart})
		for _, deviceID := range []string{"SYNC1", "SYNC2"} {
			code, _, errcode := client.do(http.MethodPost, "/_synapse/mas/upsert_device", map[string]any{
				"localpart": localpart,
				"device_id": deviceID,
			})
			if code != http.StatusCreated {
				t.Fatalf("upsert_device %s: expected 201, got %d (%s)", deviceID, code, errcode)
			}
		}

		// Store a dehydrated device outside of MAS.
		var storeRes userapiAPI.PerformStoreDehydratedDeviceResponse
		if err := userAPI.PerformStoreDehydratedDevice(t.Context(), &userapiAPI.PerformStoreDehydratedDeviceRequest{
			UserID:     userID,
			DeviceID:   "DEHYDRATED1",
			DeviceData: json.RawMessage(`{"algorithm":"org.example.dehydration.v1"}`),
		}, &storeRes); err != nil {
			t.Fatalf("failed to store dehydrated device: %v", err)
		}

		// Sync to only SYNC2: SYNC1 must go, the dehydrated device must stay.
		code, raw, errcode := client.do(http.MethodPost, "/_synapse/mas/sync_devices", map[string]any{
			"localpart": localpart,
			"devices":   []string{"SYNC2"},
		})
		if code != http.StatusOK {
			t.Fatalf("sync_devices: expected 200, got %d %s (%s)", code, raw, errcode)
		}

		var devices userapiAPI.QueryDevicesResponse
		if err := userAPI.QueryDevices(t.Context(), &userapiAPI.QueryDevicesRequest{UserID: userID}, &devices); err != nil {
			t.Fatalf("QueryDevices failed: %v", err)
		}
		got := map[string]bool{}
		for _, d := range devices.Devices {
			got[d.ID] = true
		}
		if got["SYNC1"] {
			t.Error("SYNC1 should have been deleted by sync_devices")
		}
		if !got["SYNC2"] {
			t.Error("SYNC2 should have survived sync_devices")
		}
		if !got["DEHYDRATED1"] {
			t.Error("dehydrated device must survive sync_devices")
		}

		// The dehydrated device record itself is also intact.
		var dehyd userapiAPI.QueryDehydratedDeviceResponse
		if err := userAPI.QueryDehydratedDevice(t.Context(), &userapiAPI.QueryDehydratedDeviceRequest{UserID: userID}, &dehyd); err != nil {
			t.Fatalf("QueryDehydratedDevice failed: %v", err)
		}
		if !dehyd.Found || dehyd.DeviceID != "DEHYDRATED1" {
			t.Errorf("dehydrated device record = found:%v id:%q, want found:true id:DEHYDRATED1", dehyd.Found, dehyd.DeviceID)
		}
	})
}

// TestMASIntegration_DeleteAndReactivateUserLifecycle verifies the full
// account lifecycle across the provisioning API: a user deleted by MAS loses
// their devices and is reported deactivated, and reactivation restores the
// account for new logins.
func TestMASIntegration_DeleteAndReactivateUserLifecycle(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		client, userAPI, introspection, cfg, cleanup := setupMASIntegrationRig(t, dbType)
		defer cleanup()

		const localpart = "lifecycle"
		userID := fmt.Sprintf("@%s:%s", localpart, cfg.ClientAPI.Matrix.ServerName)

		client.provisionUser(map[string]any{"localpart": localpart})
		if code, _, errcode := client.do(http.MethodPost, "/_synapse/mas/upsert_device", map[string]any{
			"localpart": localpart,
			"device_id": "LIFE1",
		}); code != http.StatusCreated {
			t.Fatalf("upsert_device: expected 201, got %d (%s)", code, errcode)
		}

		// Bind a real token and verify it resolves.
		const realToken = "syz_lifecycle_token"
		introspection.set(realToken, introspectionForDevice(localpart, "LIFE1"))
		assertTokenResolves := func(wantDevice bool) {
			t.Helper()
			var res userapiAPI.QueryAccessTokenResponse
			if err := userAPI.QueryAccessToken(t.Context(), &userapiAPI.QueryAccessTokenRequest{AccessToken: realToken}, &res); err != nil {
				t.Fatalf("QueryAccessToken failed: %v", err)
			}
			if wantDevice && res.Device == nil {
				t.Fatal("expected device for active token")
			}
			if !wantDevice && res.Device != nil {
				t.Fatalf("expected no device after deletion, got %+v", res.Device)
			}
		}
		assertTokenResolves(true)

		// MAS deletes the user.
		code, raw, errcode := client.do(http.MethodPost, "/_synapse/mas/delete_user", map[string]any{
			"localpart": localpart,
			"erase":     false,
		})
		if code != http.StatusOK {
			t.Fatalf("delete_user: expected 200, got %d %s (%s)", code, raw, errcode)
		}

		// The account reports deactivated, the devices are gone.
		_, body, _ := client.queryUser(localpart)
		if deactivated, _ := body["is_deactivated"].(bool); !deactivated {
			t.Error("query_user is_deactivated must be true after delete_user")
		}
		var devices userapiAPI.QueryDevicesResponse
		if err := userAPI.QueryDevices(t.Context(), &userapiAPI.QueryDevicesRequest{UserID: userID}, &devices); err != nil {
			t.Fatalf("QueryDevices failed: %v", err)
		}
		if len(devices.Devices) != 0 {
			t.Errorf("expected no devices after delete_user, got %+v", devices.Devices)
		}

		// MAS reactivates the account.
		code, raw, errcode = client.do(http.MethodPost, "/_synapse/mas/reactivate_user", map[string]any{
			"localpart": localpart,
		})
		if code != http.StatusOK {
			t.Fatalf("reactivate_user: expected 200, got %d %s (%s)", code, raw, errcode)
		}
		_, body, _ = client.queryUser(localpart)
		if deactivated, _ := body["is_deactivated"].(bool); deactivated {
			t.Error("query_user is_deactivated must be false after reactivate_user")
		}

		// A fresh login works again: MAS provisions a new device, the client
		// binds a new token.
		if code, _, errcode := client.do(http.MethodPost, "/_synapse/mas/upsert_device", map[string]any{
			"localpart": localpart,
			"device_id": "LIFE2",
		}); code != http.StatusCreated {
			t.Fatalf("upsert_device after reactivation: expected 201, got %d (%s)", code, errcode)
		}
		introspection.set("syz_lifecycle_token_2", introspectionForDevice(localpart, "LIFE2"))
		var res userapiAPI.QueryAccessTokenResponse
		if err := userAPI.QueryAccessToken(t.Context(), &userapiAPI.QueryAccessTokenRequest{AccessToken: "syz_lifecycle_token_2"}, &res); err != nil {
			t.Fatalf("QueryAccessToken failed: %v", err)
		}
		if res.Device == nil || res.Device.ID != "LIFE2" {
			t.Errorf("expected device LIFE2 after fresh login, got %+v", res.Device)
		}
	})
}
