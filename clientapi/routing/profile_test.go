// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/setup/config"
)

type fakeProfileFailures map[spec.ServerName]bool

func (f fakeProfileFailures) RemoteProfileFailed(serverName spec.ServerName) bool {
	return f[serverName]
}

func (f fakeProfileFailures) StoreRemoteProfileFailure(serverName spec.ServerName) {
	f[serverName] = true
}

type profileFederation struct {
	fclient.FederationClient
	t       *testing.T
	profile string
	err     error
	calls   int
}

func (f *profileFederation) DoRequestAndParseResponse(_ context.Context, req *http.Request, result any) error {
	require.Equal(f.t, http.MethodGet, req.Method)
	require.Equal(f.t, "remote.test", req.URL.Host)
	require.Equal(f.t, "/_matrix/federation/v1/query/profile", req.URL.Path)
	require.Equal(f.t, "@bob:remote.test", req.URL.Query().Get("user_id"))
	require.Empty(f.t, req.URL.Query().Get("field"))
	require.Contains(f.t, req.Header.Get("Authorization"), "X-Matrix")
	f.calls++
	if f.err != nil {
		return f.err
	}
	return json.Unmarshal([]byte(f.profile), result)
}

func TestRemoteProfileFields(t *testing.T) {
	var cfg config.Zendrite
	cfg.Defaults(config.DefaultOpts{Generate: true, SingleDatabase: true})
	fed := &profileFederation{
		t:       t,
		profile: `{"displayname":"Bob","avatar_url":"mxc://remote.test/bob","m.tz":"Europe/Paris","org.example.job":{"title":"Engineer"}}`,
	}
	req := httptest.NewRequest(http.MethodGet, "/_matrix/client/v3/profile/@bob:remote.test", nil)
	res := GetProfile(req, nil, &cfg.ClientAPI, "@bob:remote.test", nil, fed, nil)
	require.Equal(t, http.StatusOK, res.Code)
	body, err := json.Marshal(res.JSON)
	require.NoError(t, err)
	require.JSONEq(t, fed.profile, string(body))

	for key, want := range map[string]string{
		"displayname":     `{"displayname":"Bob"}`,
		"avatar_url":      `{"avatar_url":"mxc://remote.test/bob"}`,
		"m.tz":            `{"m.tz":"Europe/Paris"}`,
		"org.example.job": `{"org.example.job":{"title":"Engineer"}}`,
	} {
		res = GetProfileField(req, nil, &cfg.ClientAPI, "@bob:remote.test", key, nil, fed, nil)
		require.Equal(t, http.StatusOK, res.Code, key)
		body, err = json.Marshal(res.JSON)
		require.NoError(t, err)
		require.JSONEq(t, want, string(body), key)
	}

	res = GetProfileField(req, nil, &cfg.ClientAPI, "@bob:remote.test", "org.example.unset", nil, fed, nil)
	require.Equal(t, http.StatusNotFound, res.Code)

	// A display name that is not a string is left out.
	fed.profile = `{"displayname":1,"m.tz":"Europe/Paris"}`
	res = GetProfile(req, nil, &cfg.ClientAPI, "@bob:remote.test", nil, fed, nil)
	require.Equal(t, http.StatusOK, res.Code)
	body, err = json.Marshal(res.JSON)
	require.NoError(t, err)
	require.JSONEq(t, `{"m.tz":"Europe/Paris"}`, string(body))

	// A remote 404 means the user has no profile.
	fed.err = spec.HTTPError{Code: http.StatusNotFound}
	res = GetProfile(req, nil, &cfg.ClientAPI, "@bob:remote.test", nil, fed, nil)
	require.Equal(t, http.StatusNotFound, res.Code)
}

func TestRemoteProfileUnreachable(t *testing.T) {
	var cfg config.Zendrite
	cfg.Defaults(config.DefaultOpts{Generate: true, SingleDatabase: true})
	req := httptest.NewRequest(http.MethodGet, "/_matrix/client/v3/profile/@bob:remote.test", nil)
	dialErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("i/o timeout")}

	requireBadGateway := func(t *testing.T, code int, body any) {
		t.Helper()
		require.Equal(t, http.StatusBadGateway, code)
		matrixErr, ok := body.(spec.MatrixError)
		require.True(t, ok)
		require.Equal(t, spec.ErrorUnknown, matrixErr.ErrCode)
	}

	t.Run("without cache", func(t *testing.T) {
		fed := &profileFederation{t: t, err: dialErr}
		res := GetProfile(req, nil, &cfg.ClientAPI, "@bob:remote.test", nil, fed, nil)
		requireBadGateway(t, res.Code, res.JSON)
		res = GetProfileField(req, nil, &cfg.ClientAPI, "@bob:remote.test", "displayname", nil, fed, nil)
		requireBadGateway(t, res.Code, res.JSON)
		require.Equal(t, 2, fed.calls)
	})

	t.Run("failure is cached per server", func(t *testing.T) {
		fed := &profileFederation{t: t, err: dialErr}
		failures := fakeProfileFailures{}
		res := GetProfile(req, nil, &cfg.ClientAPI, "@bob:remote.test", nil, fed, failures)
		requireBadGateway(t, res.Code, res.JSON)
		require.True(t, failures["remote.test"])

		// Later lookups fail fast without dialing out, also for the field endpoint.
		res = GetProfile(req, nil, &cfg.ClientAPI, "@bob:remote.test", nil, fed, failures)
		requireBadGateway(t, res.Code, res.JSON)
		res = GetProfileField(req, nil, &cfg.ClientAPI, "@bob:remote.test", "avatar_url", nil, fed, failures)
		requireBadGateway(t, res.Code, res.JSON)
		require.Equal(t, 1, fed.calls)
	})

	t.Run("HTTP errors are not cached", func(t *testing.T) {
		for code, want := range map[int]int{
			http.StatusNotFound:  http.StatusNotFound,
			http.StatusForbidden: http.StatusForbidden,
		} {
			fed := &profileFederation{t: t, err: spec.HTTPError{Code: code}}
			failures := fakeProfileFailures{}
			for range 2 {
				res := GetProfile(req, nil, &cfg.ClientAPI, "@bob:remote.test", nil, fed, failures)
				require.Equal(t, want, res.Code)
			}
			require.Equal(t, 2, fed.calls)
			require.Empty(t, failures)
		}
	})

	t.Run("canceled client request is not cached", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		cancel(nil)
		fed := &profileFederation{t: t, err: context.Canceled}
		failures := fakeProfileFailures{}
		res := GetProfile(req.WithContext(ctx), nil, &cfg.ClientAPI, "@bob:remote.test", nil, fed, failures)
		require.Equal(t, http.StatusBadGateway, res.Code)
		require.Empty(t, failures)
	})
}
