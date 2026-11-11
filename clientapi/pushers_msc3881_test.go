// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package clientapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/httputil"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
	"codefloe.com/pat-s/zendrite/userapi"
	uapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// TestPushersMSC3881 covers remotely toggling another device's pusher with
// MSC3881's enabled flag and device ID.
func TestPushersMSC3881(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
		defer closeDB()
		cfg.ClientAPI.RateLimiting.Enabled = false
		routers := httputil.NewRouters()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		natsInstance := jetstream.NATSInstance{}
		jsctx, _ := natsInstance.Prepare(processCtx, &cfg.Global.JetStream)
		defer jetstream.DeleteAllStreams(jsctx, &cfg.Global.JetStream)
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		rsAPI.SetFederationAPI(nil, nil)
		userAPI := userapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, rsAPI, nil, caching.DisableMetrics, testIsBlacklistedOrBackingOff)
		AddPublicRoutes(processCtx, routers, cfg, &natsInstance, nil, rsAPI, nil, nil, nil, userAPI, nil, nil, nil, caching.DisableMetrics)

		alice, bob := test.NewUser(t), test.NewUser(t)
		accessTokens := map[*test.User]userDevice{alice: {}, bob: {}}
		createAccessTokens(t, accessTokens, userAPI, processCtx.Context(), routers)
		ctx := processCtx.Context()
		phone := accessTokens[alice]

		// Alice's second device.
		laptopID := "LAPTOP"
		laptopToken := "alice_laptop_token"
		require.NoError(t, userAPI.PerformDeviceCreation(ctx, &uapi.PerformDeviceCreationRequest{
			Localpart:          alice.Localpart,
			ServerName:         cfg.Global.ServerName,
			AccessToken:        laptopToken,
			DeviceID:           &laptopID,
			NoDeviceListUpdate: true,
		}, &uapi.PerformDeviceCreationResponse{}))

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
		pusher := func(extra map[string]any) map[string]any {
			body := map[string]any{
				"kind":                "http",
				"app_id":              "com.example.app",
				"app_display_name":    "Example",
				"device_display_name": "Alice's phone",
				"pushkey":             "phone-pushkey",
				"lang":                "en",
				"data":                map[string]any{"url": "https://push.example.com/_matrix/push/v1/notify"},
			}
			for k, v := range extra {
				body[k] = v
			}
			return body
		}
		set := func(token string, body map[string]any) {
			t.Helper()
			code, res := do(http.MethodPost, "/_matrix/client/v3/pushers/set", token, body)
			require.Equal(t, http.StatusOK, code, res.Raw)
		}
		get := func(token string) []gjson.Result {
			t.Helper()
			code, res := do(http.MethodGet, "/_matrix/client/v3/pushers", token, nil)
			require.Equal(t, http.StatusOK, code, res.Raw)
			return res.Get("pushers").Array()
		}

		t.Run("enabled by default, with the registering device", func(t *testing.T) {
			set(phone.accessToken, pusher(nil))
			pushers := get(laptopToken)
			require.Len(t, pushers, 1)
			assert.Equal(t, gjson.True, pushers[0].Get(`org\.matrix\.msc3881\.enabled`).Type)
			assert.Equal(t, phone.deviceID, pushers[0].Get(`org\.matrix\.msc3881\.device_id`).String())
			assert.False(t, pushers[0].Get("session_id").Exists())
		})

		t.Run("another device disables and enables it", func(t *testing.T) {
			set(laptopToken, pusher(map[string]any{"org.matrix.msc3881.enabled": false}))
			pushers := get(phone.accessToken)
			require.Len(t, pushers, 1)
			assert.Equal(t, gjson.False, pushers[0].Get(`org\.matrix\.msc3881\.enabled`).Type)
			assert.Equal(t, phone.deviceID, pushers[0].Get(`org\.matrix\.msc3881\.device_id`).String())

			set(laptopToken, pusher(map[string]any{"org.matrix.msc3881.enabled": true}))
			pushers = get(phone.accessToken)
			require.Len(t, pushers, 1)
			assert.Equal(t, gjson.True, pushers[0].Get(`org\.matrix\.msc3881\.enabled`).Type)
			assert.Equal(t, phone.deviceID, pushers[0].Get(`org\.matrix\.msc3881\.device_id`).String())
		})

		t.Run("a client cannot choose the device", func(t *testing.T) {
			set(laptopToken, pusher(map[string]any{
				"pushkey":                      "laptop-pushkey",
				"org.matrix.msc3881.device_id": phone.deviceID,
				"org.matrix.msc3881.enabled":   nil,
				"device_display_name":          "Alice's laptop",
			}))
			pushers := get(laptopToken)
			require.Len(t, pushers, 2)
			for _, p := range pushers {
				if p.Get("pushkey").String() == "laptop-pushkey" {
					assert.Equal(t, laptopID, p.Get(`org\.matrix\.msc3881\.device_id`).String())
					// null, as in Synapse, means enabled.
					assert.Equal(t, gjson.True, p.Get(`org\.matrix\.msc3881\.enabled`).Type)
				}
			}
		})

		t.Run("enabled must be a boolean", func(t *testing.T) {
			code, res := do(http.MethodPost, "/_matrix/client/v3/pushers/set", laptopToken, pusher(map[string]any{"org.matrix.msc3881.enabled": "no"}))
			assert.Equal(t, http.StatusBadRequest, code, res.Raw)
			assert.Equal(t, "M_BAD_JSON", res.Get("errcode").String())
		})

		t.Run("deleting a device deletes its pushers", func(t *testing.T) {
			require.NoError(t, userAPI.PerformDeviceDeletion(ctx, &uapi.PerformDeviceDeletionRequest{
				UserID:    alice.ID,
				DeviceIDs: []string{laptopID},
			}, &uapi.PerformDeviceDeletionResponse{}))
			pushers := get(phone.accessToken)
			require.Len(t, pushers, 1)
			assert.Equal(t, "phone-pushkey", pushers[0].Get("pushkey").String())
		})

		t.Run("another user taking the pushkey removes it", func(t *testing.T) {
			set(accessTokens[bob].accessToken, pusher(nil))
			assert.Empty(t, get(phone.accessToken))
			pushers := get(accessTokens[bob].accessToken)
			require.Len(t, pushers, 1)
			assert.Equal(t, accessTokens[bob].deviceID, pushers[0].Get(`org\.matrix\.msc3881\.device_id`).String())
		})
	})
}
