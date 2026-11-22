// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
	"codefloe.com/pat-s/zendrite/userapi"
	uapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// TestRegisterGuestUpgrade checks that a guest becomes a user account, with
// the same user ID, by registering with its access token as
// guest_access_token and its localpart as the username, and that a guest
// cannot upgrade another guest (Sytest "Guest user can upgrade to fully
// featured user" and "Guest user cannot upgrade other users").
func TestRegisterGuestUpgrade(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
		defer closeDB()
		cfg.ClientAPI.RegistrationDisabled = false
		cfg.ClientAPI.GuestsDisabled = false
		require.NoError(t, cfg.Derive())
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		natsInstance := jetstream.NATSInstance{}
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		rsAPI.SetFederationAPI(nil, nil)
		userAPI := userapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, rsAPI, nil, caching.DisableMetrics, testIsBlacklistedOrBackingOff)

		register := func(t *testing.T, query string, body map[string]any) (int, map[string]any) {
			t.Helper()
			raw, err := json.Marshal(body)
			require.NoError(t, err)
			req := httptest.NewRequest(http.MethodPost, "/"+query, bytes.NewReader(raw))
			res := Register(req, userAPI, &cfg.ClientAPI)
			raw, err = json.Marshal(res.JSON)
			require.NoError(t, err)
			var out map[string]any
			require.NoError(t, json.Unmarshal(raw, &out))
			return res.Code, out
		}
		newGuest := func(t *testing.T) (userID, localpart, token string) {
			t.Helper()
			code, out := register(t, "?kind=guest", map[string]any{})
			require.Equal(t, http.StatusOK, code, out)
			userID, _ = out["user_id"].(string)
			token, _ = out["access_token"].(string)
			localpart, _, err := gomatrixserverlib.SplitID('@', userID)
			require.NoError(t, err)
			return userID, localpart, token
		}

		guestID, guestLocalpart, guestToken := newGuest(t)
		_, _, otherToken := newGuest(t)

		// Another guest's token cannot upgrade this guest.
		code, out := register(t, "", map[string]any{
			"username":           guestLocalpart,
			"password":           "SIR_Arthur_David",
			"guest_access_token": otherToken,
		})
		assert.Equal(t, http.StatusForbidden, code, out)

		// The first request asks for the auth flow, the second completes it.
		// As in Sytest, the second leaves out the session, which the guest's
		// access token makes unnecessary.
		upgrade := map[string]any{
			"username":           guestLocalpart,
			"password":           "SIR_Arthur_David",
			"guest_access_token": guestToken,
		}
		code, out = register(t, "", upgrade)
		require.Equal(t, http.StatusUnauthorized, code, out)
		upgrade["auth"] = map[string]any{"type": "m.login.dummy"}
		code, out = register(t, "", upgrade)
		require.Equal(t, http.StatusOK, code, out)
		assert.Equal(t, guestID, out["user_id"])

		ctx := processCtx.Context()
		var account uapi.QueryAccountByLocalpartResponse
		require.NoError(t, userAPI.QueryAccountByLocalpart(ctx, &uapi.QueryAccountByLocalpartRequest{
			Localpart: guestLocalpart, ServerName: cfg.Global.ServerName,
		}, &account))
		assert.Equal(t, uapi.AccountTypeUser, account.Account.AccountType)

		// The guest's own access token now belongs to the user account.
		var token uapi.QueryAccessTokenResponse
		require.NoError(t, userAPI.QueryAccessToken(ctx, &uapi.QueryAccessTokenRequest{AccessToken: guestToken}, &token))
		require.NotNil(t, token.Device)
		assert.Equal(t, uapi.AccountTypeUser, token.Device.AccountType)

		// The password works.
		var login uapi.QueryAccountByPasswordResponse
		require.NoError(t, userAPI.QueryAccountByPassword(ctx, &uapi.QueryAccountByPasswordRequest{
			Localpart: guestLocalpart, ServerName: cfg.Global.ServerName, PlaintextPassword: "SIR_Arthur_David",
		}, &login))
		assert.True(t, login.Exists)

		// A user account cannot be upgraded again with the same token.
		code, out = register(t, "", map[string]any{
			"username":           guestLocalpart,
			"password":           "another_password",
			"guest_access_token": guestToken,
		})
		assert.Equal(t, http.StatusForbidden, code, out)
		assert.Equal(t, string(spec.ErrorForbidden), out["errcode"])
	})
}
