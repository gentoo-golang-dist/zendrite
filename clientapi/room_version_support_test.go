// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package clientapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/appservice"
	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/httputil"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	"codefloe.com/pat-s/zendrite/setup/base"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
	"codefloe.com/pat-s/zendrite/userapi"
)

// The MSC4242 state-DAG room version is known to gomatrixserverlib but not
// implemented, so it is neither advertised nor accepted for new rooms or
// upgrades. Implemented versions are unaffected.
func TestUnimplementedRoomVersionRefused(t *testing.T) {
	alice := test.NewUser(t)
	stateDAGs := string(gomatrixserverlib.RoomVersionStateDAGs)

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
		defer closeDB()
		cfg.ClientAPI.RateLimiting.Enabled = false
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		routers := httputil.NewRouters()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		natsInstance := &jetstream.NATSInstance{}

		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, natsInstance, caches, caching.DisableMetrics)
		rsAPI.SetFederationAPI(nil, nil)
		userAPI := userapi.NewInternalAPI(processCtx, cfg, cm, natsInstance, rsAPI, nil, caching.DisableMetrics, testIsBlacklistedOrBackingOff)
		asAPI := appservice.NewInternalAPI(processCtx, cfg, natsInstance, userAPI, rsAPI)
		AddPublicRoutes(processCtx, routers, cfg, natsInstance, base.CreateFederationClient(cfg, nil), rsAPI, asAPI, nil, nil, userAPI, nil, nil, nil, caching.DisableMetrics)

		accessTokens := map[*test.User]userDevice{alice: {}}
		createAccessTokens(t, accessTokens, userAPI, processCtx.Context(), routers)

		do := func(method, path, body string) *httptest.ResponseRecorder {
			t.Helper()
			req := httptest.NewRequest(method, path, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+accessTokens[alice].accessToken)
			rec := httptest.NewRecorder()
			routers.Client.ServeHTTP(rec, req)
			return rec
		}

		rec := do(http.MethodGet, "/_matrix/client/v3/capabilities", "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		available := gjson.GetBytes(rec.Body.Bytes(), `capabilities.m\.room_versions.available`)
		assert.False(t, available.Get(gjson.Escape(stateDAGs)).Exists(), available.Raw)
		assert.Equal(t, "stable", available.Get("12").Str, available.Raw)
		assert.Equal(t, "unstable", available.Get(gjson.Escape(string(gomatrixserverlib.RoomVersionHydra))).Str, available.Raw)

		rec = do(http.MethodPost, "/_matrix/client/v3/createRoom", `{"room_version":"`+stateDAGs+`"}`)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Equal(t, "M_UNSUPPORTED_ROOM_VERSION", gjson.Get(rec.Body.String(), "errcode").Str)

		rec = do(http.MethodPost, "/_matrix/client/v3/createRoom", `{"room_version":"12"}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		roomID := gjson.Get(rec.Body.String(), "room_id").Str

		rec = do(http.MethodPost, "/_matrix/client/v3/rooms/"+roomID+"/upgrade", `{"new_version":"`+stateDAGs+`"}`)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Equal(t, "M_UNSUPPORTED_ROOM_VERSION", gjson.Get(rec.Body.String(), "errcode").Str)
	})
}
