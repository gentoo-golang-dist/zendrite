// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jellydator/ttlcache/v3"
	"github.com/matrix-org/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	capi "codefloe.com/pat-s/zendrite/clientapi/api"
	"codefloe.com/pat-s/zendrite/clientapi/auth/authtypes"
	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
	"codefloe.com/pat-s/zendrite/userapi"
	"codefloe.com/pat-s/zendrite/userapi/api"
)

// registerWithDummy registers as Element Call does: one request to learn the
// flows, then one completing m.login.dummy.
func registerWithDummy(t *testing.T, userAPI api.ClientUserAPI, cfg *config.ClientAPI, username string) util.JSONResponse {
	t.Helper()
	body := &bytes.Buffer{}
	require.NoError(t, json.NewEncoder(body).Encode(registerRequest{Username: username, Password: "0123456789abcdef"}))
	resp := Register(httptest.NewRequest(http.MethodPost, "/?kind=user", body), userAPI, cfg)
	uia, ok := resp.JSON.(userInteractiveResponse)
	if !ok {
		return resp
	}
	assert.Contains(t, uia.Flows, authtypes.Flow{Stages: []authtypes.LoginType{authtypes.LoginTypeDummy}})
	body.Reset()
	require.NoError(t, json.NewEncoder(body).Encode(registerRequest{
		Username: username,
		Password: "0123456789abcdef",
		Auth:     authDict{Type: authtypes.LoginTypeDummy, Session: uia.Session},
	}))
	return Register(httptest.NewRequest(http.MethodPost, "/?kind=user", body), userAPI, cfg)
}

func TestRegisterCallGuest(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeFn := testrig.CreateConfig(t, dbType)
		defer closeFn()
		cfg.Global.ServerName = "test"
		cfg.ClientAPI.RegistrationDisabled = true
		cfg.ClientAPI.RegistrationSharedSecret = "zendritetest"
		cfg.ClientAPI.RegistrationRequiresToken = true

		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		natsInstance := jetstream.NATSInstance{}
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		rsAPI.SetFederationAPI(nil, nil)
		userAPI := userapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, rsAPI, nil, caching.DisableMetrics, testIsBlacklistedOrBackingOff)
		ctx := processCtx.Context()

		accountType := func(t *testing.T, localpart string) api.AccountType {
			t.Helper()
			res := api.QueryAccountByLocalpartResponse{}
			require.NoError(t, userAPI.QueryAccountByLocalpart(ctx, &api.QueryAccountByLocalpartRequest{Localpart: localpart, ServerName: "test"}, &res))
			return res.Account.AccountType
		}
		derive := func(t *testing.T, callGuests bool) {
			t.Helper()
			cfg.ClientAPI.CallGuests.Enable = callGuests
			require.NoError(t, cfg.Derive())
		}

		t.Run("refused while call guests are disabled", func(t *testing.T) {
			derive(t, false)
			resp := registerWithDummy(t, userAPI, &cfg.ClientAPI, "refused")
			assert.Equal(t, http.StatusForbidden, resp.Code)
		})

		t.Run("dummy auth creates a call guest", func(t *testing.T) {
			derive(t, true)
			resp := registerWithDummy(t, userAPI, &cfg.ClientAPI, "callguest")
			registered, ok := resp.JSON.(registerResponse)
			require.True(t, ok, "%+v", resp)
			assert.Equal(t, "@callguest:test", registered.UserID)
			assert.NotEmpty(t, registered.AccessToken)
			assert.Equal(t, api.AccountTypeCallGuest, accountType(t, "callguest"))

			// The display name defaults to the localpart, as for users.
			profile, err := userAPI.QueryProfile(ctx, "@callguest:test")
			require.NoError(t, err)
			assert.Equal(t, "callguest", profile.DisplayName)
		})

		t.Run("a registration token still creates a user", func(t *testing.T) {
			derive(t, true)
			_, err := userAPI.PerformAdminCreateRegistrationToken(ctx, &capi.RegistrationToken{
				Token: ptrTo("calltoken"), Pending: ptrTo(int32(0)), Completed: ptrTo(int32(0)),
			})
			require.NoError(t, err)
			resp := registerWithToken(t, userAPI, &cfg.ClientAPI, "tokenuser", "password-token", "calltoken")
			_, ok := resp.JSON.(registerResponse)
			require.True(t, ok, "%+v", resp)
			assert.Equal(t, api.AccountTypeUser, accountType(t, "tokenuser"))
		})

		t.Run("shared-secret registration still creates an admin", func(t *testing.T) {
			derive(t, true)
			jsonStr := []byte(`{"admin":true,"mac":"8a433a45485068a83bfb36687641c98bfbd34eca","nonce":"759f047f312b99ff428b21d581256f8592b8976e58bc1b543972dc6147e529a79657605b52d7becd160ff5137f3de11975684319187e06901955f79e5a6c5a79","password":"wonderland","username":"alice","displayname":"rabbit"}`)
			req, err := NewSharedSecretRegistrationRequest(io.NopCloser(bytes.NewReader(jsonStr)))
			require.NoError(t, err)
			sr := NewSharedSecretRegistration(cfg.ClientAPI.RegistrationSharedSecret)
			sr.nonces.Set(req.Nonce, true, ttlcache.DefaultTTL)
			resp := handleSharedSecretRegistration(&cfg.ClientAPI, userAPI, sr, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(jsonStr)))
			require.Equal(t, http.StatusOK, resp.Code, "%+v", resp)
			assert.Equal(t, api.AccountTypeAdmin, accountType(t, "alice"))
		})

		t.Run("guest registration is unchanged", func(t *testing.T) {
			derive(t, true)
			resp := Register(httptest.NewRequest(http.MethodPost, "/?kind=guest", bytes.NewBufferString("{}")), userAPI, &cfg.ClientAPI)
			assert.Equal(t, http.StatusForbidden, resp.Code)
		})
	})
}
