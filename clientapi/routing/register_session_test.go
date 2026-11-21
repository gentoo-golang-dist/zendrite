// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/matrix-org/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/clientapi/auth/authtypes"
	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
	"codefloe.com/pat-s/zendrite/userapi"
	"codefloe.com/pat-s/zendrite/userapi/api"
)

// TestRegisterRequiresIssuedSession checks that once /register issued a UIA
// session, an auth stage only counts when the client passes it back
// (Complement's "Registration without a session fails").
func TestRegisterRequiresIssuedSession(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeFn := testrig.CreateConfig(t, dbType)
		defer closeFn()
		cfg.Global.ServerName = "test"
		cfg.ClientAPI.RegistrationDisabled = false
		cfg.ClientAPI.GuestsDisabled = false
		require.NoError(t, cfg.Derive())

		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		natsInstance := jetstream.NATSInstance{}
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		rsAPI.SetFederationAPI(nil, nil)
		userAPI := userapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, rsAPI, nil, caching.DisableMetrics, testIsBlacklistedOrBackingOff)

		const password = "0123456789abcdef"
		// The backends run in parallel and share the registration sessions,
		// so each needs its own usernames.
		name := func(username string) string { return fmt.Sprintf("%s%d", username, dbType) }
		register := func(t *testing.T, r registerRequest) util.JSONResponse {
			t.Helper()
			body := &bytes.Buffer{}
			require.NoError(t, json.NewEncoder(body).Encode(r))
			return Register(httptest.NewRequest(http.MethodPost, "/", body), userAPI, &cfg.ClientAPI)
		}
		startSession := func(t *testing.T, username string) string {
			t.Helper()
			resp := register(t, registerRequest{Username: username, Password: password})
			require.Equal(t, http.StatusUnauthorized, resp.Code)
			uia, ok := resp.JSON.(userInteractiveResponse)
			require.True(t, ok, "%+v", resp)
			require.NotEmpty(t, uia.Session)
			return uia.Session
		}
		accountExists := func(t *testing.T, localpart string) bool {
			t.Helper()
			res := api.QueryAccountAvailabilityResponse{}
			require.NoError(t, userAPI.QueryAccountAvailability(processCtx.Context(), &api.QueryAccountAvailabilityRequest{
				Localpart: localpart, ServerName: "test",
			}, &res))
			return !res.Available
		}
		registered := func(t *testing.T, resp util.JSONResponse) registerResponse {
			t.Helper()
			res, ok := resp.JSON.(registerResponse)
			require.True(t, ok, "%+v", resp)
			return res
		}
		// requireFreshChallenge asserts a 401 UIA response with a new session
		// and no completed stages.
		requireFreshChallenge := func(t *testing.T, resp util.JSONResponse, notSession string) {
			t.Helper()
			require.Equal(t, http.StatusUnauthorized, resp.Code, "%+v", resp)
			uia, ok := resp.JSON.(userInteractiveResponse)
			require.True(t, ok, "%+v", resp)
			assert.NotEmpty(t, uia.Session)
			assert.NotEqual(t, notSession, uia.Session)
			assert.Empty(t, uia.Completed)
		}

		t.Run("auth without the issued session is refused", func(t *testing.T) {
			issued := startSession(t, name("nosession"))
			resp := register(t, registerRequest{
				Username: name("nosession"), Password: password,
				Auth: authDict{Type: authtypes.LoginTypeDummy},
			})
			requireFreshChallenge(t, resp, issued)
			assert.False(t, accountExists(t, name("nosession")))

			// Leaving the session out again is refused again.
			resp = register(t, registerRequest{
				Username: name("nosession"), Password: password,
				Auth: authDict{Type: authtypes.LoginTypeDummy},
			})
			requireFreshChallenge(t, resp, issued)
			assert.False(t, accountExists(t, name("nosession")))

			// The session from the refusal completes the registration.
			uia, ok := resp.JSON.(userInteractiveResponse)
			require.True(t, ok, "%+v", resp)
			retry := uia.Session
			resp = register(t, registerRequest{
				Username: name("nosession"), Password: password,
				Auth: authDict{Type: authtypes.LoginTypeDummy, Session: retry},
			})
			require.Equal(t, http.StatusOK, resp.Code, "%+v", resp)
			assert.Equal(t, "@"+name("nosession")+":test", registered(t, resp).UserID)
		})

		t.Run("auth with a session the server never issued is refused", func(t *testing.T) {
			resp := register(t, registerRequest{
				Username: name("madeup"), Password: password,
				Auth: authDict{Type: authtypes.LoginTypeDummy, Session: "client-chosen-session"},
			})
			requireFreshChallenge(t, resp, "client-chosen-session")
			assert.False(t, accountExists(t, name("madeup")))
		})

		t.Run("a client that never received a session registers in one request", func(t *testing.T) {
			// Complement's blueprints register users this way.
			startSession(t, name("otheruser"))
			resp := register(t, registerRequest{
				Username: name("singleshot"), Password: password,
				Auth: authDict{Type: authtypes.LoginTypeDummy},
			})
			require.Equal(t, http.StatusOK, resp.Code, "%+v", resp)
			assert.Equal(t, "@"+name("singleshot")+":test", registered(t, resp).UserID)
		})

		t.Run("an expired session no longer blocks the username", func(t *testing.T) {
			issued := startSession(t, name("expired"))
			sessions.deleteSession(issued)
			resp := register(t, registerRequest{
				Username: name("expired"), Password: password,
				Auth: authDict{Type: authtypes.LoginTypeDummy},
			})
			require.Equal(t, http.StatusOK, resp.Code, "%+v", resp)
		})

		t.Run("auth with the issued session registers", func(t *testing.T) {
			session := startSession(t, name("withsession"))
			resp := register(t, registerRequest{
				Username: name("withsession"), Password: password,
				Auth: authDict{Type: authtypes.LoginTypeDummy, Session: session},
			})
			require.Equal(t, http.StatusOK, resp.Code, "%+v", resp)
			assert.Equal(t, "@"+name("withsession")+":test", registered(t, resp).UserID)
			// The completed flow no longer holds the username.
			assert.False(t, sessions.hasSessionForUsername(registerRequest{Username: name("withsession"), ServerName: "test"}))

			// Replaying the session returns the completed registration.
			resp = register(t, registerRequest{
				Auth: authDict{Type: authtypes.LoginTypeDummy, Session: session},
			})
			require.Equal(t, http.StatusOK, resp.Code, "%+v", resp)
			assert.Equal(t, "@"+name("withsession")+":test", registered(t, resp).UserID)
		})

		t.Run("inhibit_login with the issued session registers", func(t *testing.T) {
			session := startSession(t, name("inhibited"))
			resp := register(t, registerRequest{
				Username: name("inhibited"), Password: password, InhibitLogin: true,
				Auth: authDict{Type: authtypes.LoginTypeDummy, Session: session},
			})
			require.Equal(t, http.StatusOK, resp.Code, "%+v", resp)
			inhibited := registered(t, resp)
			assert.Equal(t, "@"+name("inhibited")+":test", inhibited.UserID)
			assert.Empty(t, inhibited.AccessToken)
		})

		t.Run("guest registration needs no session", func(t *testing.T) {
			body := bytes.NewBufferString(`{}`)
			resp := Register(httptest.NewRequest(http.MethodPost, "/?kind=guest", body), userAPI, &cfg.ClientAPI)
			require.Equal(t, http.StatusOK, resp.Code, "%+v", resp)
			assert.NotEmpty(t, registered(t, resp).AccessToken)
		})
	})
}
