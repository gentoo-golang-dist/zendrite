// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package clientapi

import (
	"crypto/ed25519"
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

// TestInviteFiltering covers invite filtering (MSC4155) on every endpoint
// that invites a local user: blocked invites are refused with 403
// M_INVITE_BLOCKED, ignored and allowed ones go through.
func TestInviteFiltering(t *testing.T) {
	alice := test.NewUser(t)
	bob := test.NewUser(t)
	charlie := test.NewUser(t)
	_, remoteKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	remote := test.NewUser(t, test.WithSigningServer("remote.example", "ed25519:remote", remoteKey))

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
		accessTokens := map[*test.User]userDevice{alice: {}, bob: {}, charlie: {}}
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
		createRoom := func(user *test.User) string {
			t.Helper()
			code, res := do(user, http.MethodPost, "/_matrix/client/v3/createRoom", map[string]any{})
			require.Equal(t, http.StatusOK, code, res.Raw)
			return "/_matrix/client/v3/rooms/" + url.PathEscape(res.Get("room_id").Str)
		}
		// invite sends an invite to bob from user over every endpoint, each
		// time into a fresh room, and returns the responses.
		type response struct {
			code int
			res  gjson.Result
		}
		invite := func(user *test.User) map[string]response {
			t.Helper()
			out := map[string]response{}
			code, res := do(user, http.MethodPost, createRoom(user)+"/invite", map[string]any{"user_id": bob.ID})
			out["invite"] = response{code, res}
			code, res = do(user, http.MethodPut, createRoom(user)+"/state/m.room.member/"+url.PathEscape(bob.ID), map[string]any{"membership": spec.Invite})
			out["state"] = response{code, res}
			code, res = do(user, http.MethodPost, "/_matrix/client/v3/createRoom", map[string]any{"invite": []string{bob.ID}})
			out["createRoom"] = response{code, res}
			return out
		}
		// federated reports whether an invite from user over federation is
		// refused as blocked.
		federatedBlocked := func(user *test.User) bool {
			t.Helper()
			room := test.NewRoom(t, user)
			ev := room.CreateEvent(t, user, spec.MRoomMember, map[string]any{"membership": spec.Invite}, test.WithStateKey(bob.ID))
			var blocked rsapi.ErrInviteBlocked
			err := rsAPI.HandleInvite(processCtx.Context(), ev)
			if errors.As(err, &blocked) {
				return true
			}
			require.NoError(t, err)
			return false
		}
		requireBlocked := func(user *test.User) {
			t.Helper()
			for endpoint, r := range invite(user) {
				require.Equal(t, http.StatusForbidden, r.code, "%s: %s", endpoint, r.res.Raw)
				require.Equal(t, "M_INVITE_BLOCKED", r.res.Get("errcode").Str, "%s: %s", endpoint, r.res.Raw)
			}
			require.True(t, federatedBlocked(user), "federation")
		}
		requireAccepted := func(user *test.User) {
			t.Helper()
			for endpoint, r := range invite(user) {
				require.Equal(t, http.StatusOK, r.code, "%s: %s", endpoint, r.res.Raw)
			}
			require.False(t, federatedBlocked(user), "federation")
		}

		msc4155 := uapi.MSC4155InvitePermissionConfigType

		t.Run("blocked user", func(t *testing.T) {
			setConfig(msc4155, `{"blocked_users":["`+alice.ID+`"]}`)
			defer setConfig(msc4155, `{}`)
			requireBlocked(alice)
			requireAccepted(charlie)
		})

		t.Run("blocked server with allowed user", func(t *testing.T) {
			setConfig(msc4155, `{"allowed_users":["`+charlie.ID+`"],"blocked_servers":["*"]}`)
			defer setConfig(msc4155, `{}`)
			requireBlocked(alice)
			requireAccepted(charlie)
		})

		t.Run("user glob", func(t *testing.T) {
			setConfig(msc4155, `{"blocked_users":["@`+alice.Localpart[:1]+`*:test"],"allowed_users":["`+charlie.ID+`"]}`)
			defer setConfig(msc4155, `{}`)
			requireBlocked(alice)
			requireAccepted(charlie)
		})

		// Ignored invites are accepted; /sync hides them (syncapi tests).
		t.Run("ignored", func(t *testing.T) {
			setConfig(msc4155, `{"ignored_users":["`+alice.ID+`"],"blocked_servers":["*"]}`)
			defer setConfig(msc4155, `{}`)
			requireAccepted(alice)
			requireBlocked(charlie)
		})

		// Server globs match the inviting server over federation, with or
		// without its port.
		t.Run("remote server", func(t *testing.T) {
			setConfig(msc4155, `{"blocked_servers":["remote.*"]}`)
			defer setConfig(msc4155, `{}`)
			require.True(t, federatedBlocked(remote))
			requireAccepted(alice)
			setConfig(msc4155, `{"ignored_servers":["remote.example"],"blocked_servers":["*"]}`)
			require.False(t, federatedBlocked(remote))
			setConfig(msc4155, `{"allowed_users":["`+remote.ID+`"],"blocked_servers":["*"]}`)
			require.False(t, federatedBlocked(remote))
		})

		t.Run("disabled", func(t *testing.T) {
			setConfig(msc4155, `{"enabled":false,"blocked_servers":["*"]}`)
			defer setConfig(msc4155, `{}`)
			requireAccepted(alice)
		})

		// A stated MSC4380 preference takes precedence, as in Synapse.
		t.Run("MSC4380 takes precedence", func(t *testing.T) {
			setConfig(msc4155, `{"blocked_servers":["*"]}`)
			defer setConfig(msc4155, `{}`)
			setConfig(uapi.InvitePermissionConfigType, `{"default_action":"allow"}`)
			requireAccepted(alice)
			setConfig(uapi.InvitePermissionConfigType, `{"default_action":"block"}`)
			setConfig(msc4155, `{"allowed_users":["*"]}`)
			requireBlocked(alice)
			// An empty one states no preference.
			setConfig(uapi.InvitePermissionConfigType, `{}`)
			setConfig(msc4155, `{"blocked_servers":["*"]}`)
			requireBlocked(alice)
		})

		// Bob's filter does not restrict his own invites.
		code, res := do(bob, http.MethodPost, "/_matrix/client/v3/createRoom", map[string]any{"invite": []string{alice.ID}})
		require.Equal(t, http.StatusOK, code, res.Raw)
	})
}
