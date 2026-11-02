// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package clientapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/httputil"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	rsapi "codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
	"codefloe.com/pat-s/zendrite/userapi"
	uapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// TestInviteBlocking covers invite blocking (MSC4380) on every endpoint that
// invites a local user.
func TestInviteBlocking(t *testing.T) {
	alice := test.NewUser(t)
	bob := test.NewUser(t)

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
		cfg.ClientAPI.RateLimiting.Enabled = false
		defer closeDB()
		routers := httputil.NewRouters()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		natsInstance := jetstream.NATSInstance{}
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		rsAPI.SetFederationAPI(nil, nil)
		userAPI := userapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, rsAPI, nil, caching.DisableMetrics, testIsBlacklistedOrBackingOff)
		rsAPI.SetUserAPI(userAPI)
		AddPublicRoutes(processCtx, routers, cfg, &natsInstance, nil, rsAPI, nil, nil, nil, userAPI, nil, nil, nil, caching.DisableMetrics)
		accessTokens := map[*test.User]userDevice{alice: {}, bob: {}}
		createAccessTokens(t, accessTokens, userAPI, processCtx.Context(), routers)

		do := func(user *test.User, method, path string, body any) (int, gjson.Result) {
			t.Helper()
			req := test.NewRequest(t, method, path, test.WithJSONBody(t, body))
			req.Header.Set("Authorization", "Bearer "+accessTokens[user].accessToken)
			rec := httptest.NewRecorder()
			routers.Client.ServeHTTP(rec, req)
			return rec.Code, gjson.ParseBytes(rec.Body.Bytes())
		}
		setConfig := func(dataType, content string) {
			t.Helper()
			code, res := do(bob, http.MethodPut, "/_matrix/client/v3/user/"+url.PathEscape(bob.ID)+"/account_data/"+dataType, gjson.Parse(content).Value())
			require.Equal(t, http.StatusOK, code, res.Raw)
		}
		requireBlocked := func(code int, res gjson.Result) {
			t.Helper()
			require.Equal(t, http.StatusForbidden, code, res.Raw)
			require.Equal(t, "M_INVITE_BLOCKED", res.Get("errcode").Str, res.Raw)
		}

		code, res := do(alice, http.MethodPost, "/_matrix/client/v3/createRoom", map[string]any{})
		require.Equal(t, http.StatusOK, code, res.Raw)
		roomID := res.Get("room_id").Str
		roomPath := "/_matrix/client/v3/rooms/" + url.PathEscape(roomID)

		// The unstable type goes first: once the stable type is set, it takes
		// precedence.
		for _, dataType := range []string{uapi.UnstableInvitePermissionConfigType, uapi.InvitePermissionConfigType} {
			t.Run("blocked by "+dataType, func(t *testing.T) {
				setConfig(dataType, `{"default_action":"block"}`)
				defer setConfig(dataType, `{}`)

				requireBlocked(do(alice, http.MethodPost, roomPath+"/invite", map[string]any{"user_id": bob.ID}))
				requireBlocked(do(alice, http.MethodPut, roomPath+"/state/m.room.member/"+url.PathEscape(bob.ID), map[string]any{"membership": spec.Invite}))
				requireBlocked(do(alice, http.MethodPost, "/_matrix/client/v3/createRoom", map[string]any{"invite": []string{bob.ID}}))

				// An invite over federation is refused the same way.
				remote := test.NewRoom(t, alice)
				invite := remote.CreateEvent(t, alice, spec.MRoomMember, map[string]any{"membership": spec.Invite}, test.WithStateKey(bob.ID))
				var blocked rsapi.ErrInviteBlocked
				require.True(t, errors.As(rsAPI.HandleInvite(processCtx.Context(), invite), &blocked))

				// Bob can still invite others.
				code, res := do(bob, http.MethodPost, "/_matrix/client/v3/createRoom", map[string]any{"invite": []string{alice.ID}})
				require.Equal(t, http.StatusOK, code, res.Raw)
			})
		}

		t.Run("other values allow invites", func(t *testing.T) {
			setConfig(uapi.InvitePermissionConfigType, `{"default_action":"allow"}`)
			code, res := do(alice, http.MethodPost, roomPath+"/invite", map[string]any{"user_id": bob.ID})
			require.Equal(t, http.StatusOK, code, res.Raw)
		})
	})
}
