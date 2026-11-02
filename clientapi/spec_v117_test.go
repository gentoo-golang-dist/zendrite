// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package clientapi

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/httputil"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
	"codefloe.com/pat-s/zendrite/userapi"
	uapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// TestAppServiceDevices covers device masquerading (MSC4326) and device
// management (MSC4190) for appservices.
func TestAppServiceDevices(t *testing.T) {
	alice := test.NewUser(t)

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
		cfg.ClientAPI.RateLimiting.Enabled = false
		defer closeDB()
		server := string(cfg.Global.ServerName)
		namespace := regexp.MustCompile(`@_bridge_.*:` + regexp.QuoteMeta(server))
		cfg.Derived.ApplicationServices = []config.ApplicationService{{
			ID:              "bridge",
			URL:             "null",
			ASToken:         "as_bridge",
			HSToken:         "hs_bridge",
			SenderLocalpart: "_bridge_bot",
			NamespaceMap: map[string][]config.ApplicationServiceNamespace{
				"users": {{Exclusive: true, Regex: namespace.String(), RegexpObject: namespace}},
			},
		}}

		routers := httputil.NewRouters()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		natsInstance := jetstream.NATSInstance{}
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		rsAPI.SetFederationAPI(nil, nil)
		userAPI := userapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, rsAPI, nil, caching.DisableMetrics, testIsBlacklistedOrBackingOff)
		AddPublicRoutes(processCtx, routers, cfg, &natsInstance, nil, rsAPI, nil, nil, nil, userAPI, nil, nil, nil, caching.DisableMetrics)
		accessTokens := map[*test.User]userDevice{alice: {}}
		createAccessTokens(t, accessTokens, userAPI, processCtx.Context(), routers)

		// The appservice's sender and a bridged user, as the appservice
		// component sets them up.
		ctx := processCtx.Context()
		for _, localpart := range []string{"_bridge_bot", "_bridge_carol"} {
			require.NoError(t, userAPI.PerformAccountCreation(ctx, &uapi.PerformAccountCreationRequest{
				AccountType:  uapi.AccountTypeAppService,
				Localpart:    localpart,
				ServerName:   cfg.Global.ServerName,
				AppServiceID: "bridge",
				OnConflict:   uapi.ConflictUpdate,
			}, &uapi.PerformAccountCreationResponse{}))
		}
		senderDevice := "_bridge_bot"
		require.NoError(t, userAPI.PerformDeviceCreation(ctx, &uapi.PerformDeviceCreationRequest{
			Localpart:          "_bridge_bot",
			ServerName:         cfg.Global.ServerName,
			AccessToken:        "as_bridge",
			DeviceID:           &senderDevice,
			NoDeviceListUpdate: true,
		}, &uapi.PerformDeviceCreationResponse{}))
		sender := "@_bridge_bot:" + server
		carol := "@_bridge_carol:" + server

		do := func(method, path, token string, body any) (int, gjson.Result) {
			t.Helper()
			var opts []test.HTTPRequestOpt
			if body != nil {
				opts = append(opts, test.WithJSONBody(t, body))
			}
			req := test.NewRequest(t, method, path, opts...)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			routers.Client.ServeHTTP(rec, req)
			return rec.Code, gjson.ParseBytes(rec.Body.Bytes())
		}
		asCarol := "?user_id=" + carol

		t.Run("unknown device", func(t *testing.T) {
			code, res := do(http.MethodGet, "/_matrix/client/v3/account/whoami"+asCarol+"&device_id=PHONE", "as_bridge", nil)
			require.Equal(t, http.StatusBadRequest, code, res.Raw)
			require.Equal(t, "M_UNKNOWN_DEVICE", res.Get("errcode").Str)
			code, res = do(http.MethodGet, "/_matrix/client/v3/account/whoami"+asCarol+"&org.matrix.msc3202.device_id=PHONE", "as_bridge", nil)
			require.Equal(t, http.StatusBadRequest, code, res.Raw)
			require.Equal(t, "ORG.MATRIX.MSC4326.M_UNKNOWN_DEVICE", res.Get("errcode").Str)
		})

		t.Run("create and use a device", func(t *testing.T) {
			code, res := do(http.MethodPut, "/_matrix/client/v3/devices/PHONE"+asCarol, "as_bridge", map[string]any{"display_name": "Bridged phone"})
			require.Equal(t, http.StatusCreated, code, res.Raw)
			// The device exists now, so this only renames it.
			code, res = do(http.MethodPut, "/_matrix/client/v3/devices/PHONE"+asCarol, "as_bridge", map[string]any{"display_name": "Phone"})
			require.Equal(t, http.StatusOK, code, res.Raw)

			for _, param := range []string{"device_id", "org.matrix.msc3202.device_id"} {
				code, res = do(http.MethodGet, "/_matrix/client/v3/account/whoami"+asCarol+"&"+param+"=PHONE", "as_bridge", nil)
				require.Equal(t, http.StatusOK, code, res.Raw)
				require.Equal(t, carol, res.Get("user_id").Str)
				require.Equal(t, "PHONE", res.Get("device_id").Str)
			}
			code, res = do(http.MethodGet, "/_matrix/client/v3/devices/PHONE"+asCarol, "as_bridge", nil)
			require.Equal(t, http.StatusOK, code, res.Raw)
			require.Equal(t, "Phone", res.Get("display_name").Str)

			// The device belongs to carol, not to the sender.
			code, res = do(http.MethodGet, "/_matrix/client/v3/account/whoami?device_id=PHONE", "as_bridge", nil)
			require.Equal(t, http.StatusBadRequest, code, res.Raw)
		})

		t.Run("sender devices", func(t *testing.T) {
			// Without user_id, the appservice acts as its sender.
			code, res := do(http.MethodPut, "/_matrix/client/v3/devices/BOT", "as_bridge", map[string]any{})
			require.Equal(t, http.StatusCreated, code, res.Raw)
			code, res = do(http.MethodGet, "/_matrix/client/v3/account/whoami?device_id=BOT", "as_bridge", nil)
			require.Equal(t, http.StatusOK, code, res.Raw)
			require.Equal(t, sender, res.Get("user_id").Str)
			require.Equal(t, "BOT", res.Get("device_id").Str)
			code, res = do(http.MethodGet, "/_matrix/client/v3/account/whoami", "as_bridge", nil)
			require.Equal(t, http.StatusOK, code, res.Raw)
			require.Equal(t, sender, res.Get("user_id").Str)
			require.Equal(t, senderDevice, res.Get("device_id").Str)
		})

		t.Run("delete devices without UIA", func(t *testing.T) {
			code, res := do(http.MethodPut, "/_matrix/client/v3/devices/TABLET"+asCarol, "as_bridge", map[string]any{})
			require.Equal(t, http.StatusCreated, code, res.Raw)
			// An empty or missing list deletes nothing.
			for _, body := range []map[string]any{{"devices": []string{}}, {}} {
				code, res = do(http.MethodPost, "/_matrix/client/v3/delete_devices"+asCarol, "as_bridge", body)
				require.Equal(t, http.StatusOK, code, res.Raw)
			}
			code, res = do(http.MethodGet, "/_matrix/client/v3/devices"+asCarol, "as_bridge", nil)
			require.Equal(t, http.StatusOK, code, res.Raw)
			require.Len(t, res.Get("devices").Array(), 2, res.Raw)
			code, res = do(http.MethodDelete, "/_matrix/client/v3/devices/PHONE"+asCarol, "as_bridge", map[string]any{})
			require.Equal(t, http.StatusOK, code, res.Raw)
			code, res = do(http.MethodPost, "/_matrix/client/v3/delete_devices"+asCarol, "as_bridge", map[string]any{"devices": []string{"TABLET"}})
			require.Equal(t, http.StatusOK, code, res.Raw)
			code, res = do(http.MethodGet, "/_matrix/client/v3/devices"+asCarol, "as_bridge", nil)
			require.Equal(t, http.StatusOK, code, res.Raw)
			require.Empty(t, res.Get("devices").Array(), res.Raw)
			code, res = do(http.MethodDelete, "/_matrix/client/v3/devices/BOT", "as_bridge", map[string]any{})
			require.Equal(t, http.StatusOK, code, res.Raw)
		})

		t.Run("regular users are unchanged", func(t *testing.T) {
			token := accessTokens[alice].accessToken
			// Only appservices create devices.
			code, res := do(http.MethodPut, "/_matrix/client/v3/devices/NEW", token, map[string]any{})
			require.Equal(t, http.StatusNotFound, code, res.Raw)
			// Deleting a device still needs UIA.
			code, res = do(http.MethodDelete, "/_matrix/client/v3/devices/"+accessTokens[alice].deviceID, token, map[string]any{})
			require.Equal(t, http.StatusUnauthorized, code, res.Raw)
			require.True(t, res.Get("flows").Exists(), res.Raw)
			// device_id is ignored for tokens that cannot assert identities.
			code, res = do(http.MethodGet, "/_matrix/client/v3/account/whoami?device_id=OTHER", token, nil)
			require.Equal(t, http.StatusOK, code, res.Raw)
			require.Equal(t, accessTokens[alice].deviceID, res.Get("device_id").Str)
		})
	})
}
