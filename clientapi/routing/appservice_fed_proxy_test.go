// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/setup/config"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// fedProxyFederation records the federation request and answers it.
type fedProxyFederation struct {
	fclient.FederationClient
	req    *http.Request
	body   string
	answer func(result any) error
}

func (f *fedProxyFederation) DoRequestAndParseResponse(_ context.Context, req *http.Request, result any) error {
	f.req = req
	if req.Body != nil {
		body, _ := io.ReadAll(req.Body)
		f.body = string(body)
	}
	return f.answer(result)
}

func TestAppserviceFederationProxy(t *testing.T) {
	_, key, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	cfg := &config.ClientAPI{Matrix: &config.Global{SigningIdentity: fclient.SigningIdentity{ServerName: "local", KeyID: "ed25519:a", PrivateKey: key}}}
	appservices := []config.ApplicationService{
		{ID: "lk", ProxyPrefix: "rtc/livekit", ProxyURL: "http://127.0.0.1:1"},
		{ID: "bridge"},
	}
	lk := &userapi.Device{UserID: "@_lk:local", AppserviceID: "lk", AccountType: userapi.AccountTypeAppService}
	ok := func(result any) error { return json.Unmarshal([]byte(`{"jwt":"token"}`), result) }

	call := func(t *testing.T, device *userapi.Device, body string, fed *fedProxyFederation) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/_matrix/client/unstable/io.element.msc4512/appservice/fed_proxy", strings.NewReader(body))
		if fed == nil {
			fed = &fedProxyFederation{answer: func(any) error {
				t.Fatal("no federation request was expected")
				return nil
			}}
		}
		res := AppserviceFederationProxy(req, device, cfg, appservices, fed)
		encoded, err := json.Marshal(res.JSON)
		require.NoError(t, err)
		var out map[string]any
		require.NoError(t, json.Unmarshal(encoded, &out))
		return res.Code, out
	}

	t.Run("post is signed, sent and relayed", func(t *testing.T) {
		fed := &fedProxyFederation{answer: ok}
		code, out := call(t, lk, `{"destination":"remote.example","method":"POST",
			"path":"/_matrix/federation/v1/rtc/livekit/get_token","query":{"a":"b c"},"body":{"room_id":"!r:remote"}}`, fed)
		require.Equal(t, http.StatusOK, code, out)
		require.Equal(t, map[string]any{"status": float64(200), "content": map[string]any{"jwt": "token"}}, out)
		require.Equal(t, http.MethodPost, fed.req.Method)
		require.Equal(t, "remote.example", fed.req.URL.Host)
		require.Equal(t, "/_matrix/federation/v1/rtc/livekit/get_token?a=b+c", fed.req.URL.RequestURI())
		require.JSONEq(t, `{"room_id":"!r:remote"}`, fed.body)
		require.Contains(t, fed.req.Header.Get("Authorization"), `X-Matrix origin="local"`)
		require.Contains(t, fed.req.Header.Get("Authorization"), `destination="remote.example"`)
	})

	t.Run("get on the unstable prefix", func(t *testing.T) {
		fed := &fedProxyFederation{answer: ok}
		code, _ := call(t, lk, `{"destination":"remote.example","method":"GET","path":"/_matrix/federation/unstable/io.element.msc4195/rtc/livekit/x"}`, fed)
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, http.MethodGet, fed.req.Method)
		require.Empty(t, fed.body)
	})

	t.Run("remote error is relayed", func(t *testing.T) {
		fed := &fedProxyFederation{answer: func(any) error {
			return spec.HTTPError{Code: http.StatusNotFound, Contents: []byte(`{"errcode":"M_UNRECOGNIZED"}`)}
		}}
		code, out := call(t, lk, `{"destination":"remote.example","method":"DELETE","path":"/_matrix/federation/v1/rtc/livekit/x"}`, fed)
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, map[string]any{"status": float64(404), "content": map[string]any{"errcode": "M_UNRECOGNIZED"}}, out)
	})

	t.Run("remote error without JSON is relayed without content", func(t *testing.T) {
		fed := &fedProxyFederation{answer: func(any) error {
			return spec.HTTPError{Code: http.StatusBadGateway, Contents: []byte(`<html>`)}
		}}
		code, out := call(t, lk, `{"destination":"remote.example","method":"GET","path":"/_matrix/federation/v1/rtc/livekit/x"}`, fed)
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, map[string]any{"status": float64(502)}, out)
	})

	t.Run("connection failure", func(t *testing.T) {
		for _, failure := range []error{
			errors.New("dial tcp: connection refused"),
			&url.Error{Op: "Get", URL: "matrix://remote.example/", Err: io.EOF},
		} {
			fed := &fedProxyFederation{answer: func(any) error { return failure }}
			code, out := call(t, lk, `{"destination":"remote.example","method":"GET","path":"/_matrix/federation/v1/rtc/livekit/x"}`, fed)
			require.Equal(t, http.StatusBadGateway, code)
			require.Equal(t, errCodeFedProxyConnectionFailed, out["errcode"])
		}
	})

	t.Run("empty success", func(t *testing.T) {
		fed := &fedProxyFederation{answer: func(any) error { return io.EOF }}
		code, out := call(t, lk, `{"destination":"remote.example","method":"PUT","path":"/_matrix/federation/v1/rtc/livekit/x","body":{}}`, fed)
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, map[string]any{"status": float64(200)}, out)
	})

	for _, tc := range []struct {
		name, body, errcode string
		code                int
	}{
		{"missing destination", `{"method":"GET","path":"/_matrix/federation/v1/rtc/livekit/x"}`, "M_MISSING_PARAM", 400},
		{"missing method", `{"destination":"remote.example","path":"/_matrix/federation/v1/rtc/livekit/x"}`, "M_MISSING_PARAM", 400},
		{"missing path", `{"destination":"remote.example","method":"GET"}`, "M_MISSING_PARAM", 400},
		{"destination not a string", `{"destination":1,"method":"GET","path":"/_matrix/federation/v1/rtc/livekit/x"}`, "M_INVALID_PARAM", 400},
		{"invalid destination", `{"destination":"not a server","method":"GET","path":"/_matrix/federation/v1/rtc/livekit/x"}`, "M_INVALID_PARAM", 400},
		{"unknown method", `{"destination":"remote.example","method":"PATCH","path":"/_matrix/federation/v1/rtc/livekit/x"}`, "M_INVALID_PARAM", 400},
		{"lowercase method", `{"destination":"remote.example","method":"get","path":"/_matrix/federation/v1/rtc/livekit/x"}`, "M_INVALID_PARAM", 400},
		{"query value not a string", `{"destination":"remote.example","method":"GET","path":"/_matrix/federation/v1/rtc/livekit/x","query":{"a":1}}`, "M_INVALID_PARAM", 400},
		{"body for get", `{"destination":"remote.example","method":"GET","path":"/_matrix/federation/v1/rtc/livekit/x","body":{}}`, "M_INVALID_PARAM", 400},
		{"body for delete", `{"destination":"remote.example","method":"DELETE","path":"/_matrix/federation/v1/rtc/livekit/x","body":{}}`, "M_INVALID_PARAM", 400},
		{"body not an object", `{"destination":"remote.example","method":"POST","path":"/_matrix/federation/v1/rtc/livekit/x","body":[1]}`, "M_INVALID_PARAM", 400},
		{"own server", `{"destination":"local","method":"GET","path":"/_matrix/federation/v1/rtc/livekit/x"}`, errCodeFedProxyDestinationDenied, 403},
		{"own server with a port", `{"destination":"local:8448","method":"GET","path":"/_matrix/federation/v1/rtc/livekit/x"}`, errCodeFedProxyDestinationDenied, 403},
		{"outside the prefix", `{"destination":"remote.example","method":"GET","path":"/_matrix/federation/v1/send/1"}`, errCodeFedProxyPathNotAllowed, 403},
		{"without version", `{"destination":"remote.example","method":"GET","path":"/_matrix/federation/rtc/livekit/x"}`, errCodeFedProxyPathNotAllowed, 403},
		{"dot segment", `{"destination":"remote.example","method":"GET","path":"/_matrix/federation/v1/rtc/livekit/../../v1/send/1"}`, errCodeFedProxyPathNotAllowed, 403},
		{"encoded dot segment", `{"destination":"remote.example","method":"GET","path":"/_matrix/federation/v1/rtc/livekit/%2e%2e/x"}`, errCodeFedProxyPathNotAllowed, 403},
		{"query in path", `{"destination":"remote.example","method":"GET","path":"/_matrix/federation/v1/rtc/livekit/x?a=b"}`, errCodeFedProxyPathNotAllowed, 403},
		{"fragment in path", `{"destination":"remote.example","method":"GET","path":"/_matrix/federation/v1/rtc/livekit/x#a"}`, errCodeFedProxyPathNotAllowed, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := call(t, lk, tc.body, nil)
			require.Equal(t, tc.code, code, out)
			require.Equal(t, tc.errcode, out["errcode"])
		})
	}

	t.Run("application service without a prefix", func(t *testing.T) {
		bridge := &userapi.Device{UserID: "@_bridge:local", AppserviceID: "bridge", AccountType: userapi.AccountTypeAppService}
		code, out := call(t, bridge, `{"destination":"remote.example","method":"GET","path":"/_matrix/federation/v1/rtc/livekit/x"}`, nil)
		require.Equal(t, http.StatusForbidden, code)
		require.Equal(t, errCodeFedProxyPathNotAllowed, out["errcode"])
	})

	t.Run("not an application service", func(t *testing.T) {
		user := &userapi.Device{UserID: "@alice:local", AccountType: userapi.AccountTypeUser}
		code, out := call(t, user, `{"destination":"remote.example","method":"GET","path":"/_matrix/federation/v1/rtc/livekit/x"}`, nil)
		require.Equal(t, http.StatusForbidden, code)
		require.Equal(t, "M_FORBIDDEN", out["errcode"])
	})

	t.Run("federation disabled", func(t *testing.T) {
		disabled := *cfg.Matrix
		disabled.DisableFederation = true
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
			`{"destination":"remote.example","method":"GET","path":"/_matrix/federation/v1/rtc/livekit/x"}`))
		res := AppserviceFederationProxy(req, lk, &config.ClientAPI{Matrix: &disabled}, appservices, nil)
		require.Equal(t, http.StatusForbidden, res.Code)
		matrixErr, isErr := res.JSON.(spec.MatrixError)
		require.True(t, isErr)
		require.EqualValues(t, errCodeFedProxyDestinationDenied, matrixErr.ErrCode)
	})
}
