// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package appserviceproxy_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/internal/appserviceproxy"
	"codefloe.com/pat-s/zendrite/internal/httputil"
	"codefloe.com/pat-s/zendrite/setup/config"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// seenRequest is what the application service received.
type seenRequest struct {
	method, uri, body string
	header            http.Header
}

// fakeAppservice records the proxied requests and answers them.
type fakeAppservice struct {
	mu   sync.Mutex
	seen []seenRequest
}

func (f *fakeAppservice) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	f.mu.Lock()
	f.seen = append(f.seen, seenRequest{method: req.Method, uri: req.URL.RequestURI(), body: string(body), header: req.Header.Clone()})
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Custom", "kept")
	w.Header().Set("Connection", "X-Hop")
	w.Header().Set("X-Hop", "dropped")
	w.Header().Set("Keep-Alive", "timeout=5")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{"jwt":"token"}`))
}

func (f *fakeAppservice) last(t *testing.T) seenRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.seen, "the application service saw no request")
	return f.seen[len(f.seen)-1]
}

func (f *fakeAppservice) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seen)
}

// appservices returns an application service registration that claims
// rtc/livekit and forwards to the fake.
func appservices(t *testing.T, fake http.Handler) []config.ApplicationService {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return []config.ApplicationService{{
		ID: "lk", HSToken: "hs_token", ProxyPrefix: "rtc/livekit", ProxyURL: srv.URL,
		ProxyHTTPClient: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}}
}

type tokenUserAPI struct {
	devices map[string]userapi.Device
}

func (u *tokenUserAPI) QueryAccessToken(_ context.Context, req *userapi.QueryAccessTokenRequest, res *userapi.QueryAccessTokenResponse) error {
	if dev, ok := u.devices[req.AccessToken]; ok {
		res.Device = &dev
	}
	return nil
}

func TestPathPattern(t *testing.T) {
	pattern := appserviceproxy.PathPattern(appserviceproxy.ClientAPI, "rtc/livekit")
	for path, want := range map[string]bool{
		"/_matrix/client/v1/rtc/livekit":                                      true,
		"/_matrix/client/v1/rtc/livekit/get_token":                            true,
		"/_matrix/client/v3/rtc/livekit/a/b":                                  true,
		"/_matrix/client/unstable/io.element.msc4195/rtc/livekit/get_token":   true,
		"/_matrix/client/rtc/livekit/get_token":                               false,
		"/_matrix/client/v1/rtc/livekitfoo":                                   false,
		"/_matrix/client/v1/rtc/other":                                        false,
		"/_matrix/client/r0/rtc/livekit/get_token":                            false,
		"/_matrix/federation/v1/rtc/livekit/get_token":                        false,
		"/_matrix/client/unstable/io.element.msc4195/x/rtc/livekit/get_token": false,
	} {
		require.Equal(t, want, pattern.MatchString(path), path)
	}
}

func TestHasDotSegments(t *testing.T) {
	for path, want := range map[string]bool{
		"/_matrix/client/v1/rtc/livekit/get_token":        false,
		"/_matrix/client/v1/rtc/livekit/../../v3/sync":    true,
		"/_matrix/client/v1/rtc/livekit/./get_token":      true,
		"/_matrix/client/v1/rtc/livekit/%2e%2e/v3/sync":   true,
		"/_matrix/client/v1/rtc/livekit/%2E/get_token":    true,
		"/_matrix/client/v1/rtc/livekit/a%2f..%2fb":       true,
		"/_matrix/client/v1/rtc/livekit/%252e%252e/token": false,
		"/_matrix/client/v1/rtc/livekit/...":              false,
		"/_matrix/client/v1/rtc/livekit/%zz":              true,
	} {
		require.Equal(t, want, appserviceproxy.HasDotSegments(path), path)
	}
}

func TestClientProxy(t *testing.T) {
	fake := &fakeAppservice{}
	ases := appservices(t, fake)
	userAPI := &tokenUserAPI{devices: map[string]userapi.Device{
		"alice_token": {UserID: "@alice:local", ID: "DEV", AccountType: userapi.AccountTypeUser},
		"guest_token": {UserID: "@guest:local", ID: "GDEV", AccountType: userapi.AccountTypeGuest},
	}}
	router := httputil.NewRouter(httputil.PublicClientPathPrefix)
	builtIn := false
	router.Handle("/v1/rtc/livekit/get_token", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		builtIn = true
		w.WriteHeader(http.StatusTeapot)
	})).Methods(http.MethodPost)
	router.Handle("/v3/sync", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})).Methods(http.MethodGet)
	rateLimits := httputil.NewRateLimits(&config.RateLimiting{Enabled: false})
	claimed := appserviceproxy.RegisterClient(router, ases, userAPI, rateLimits, false)
	require.Len(t, claimed, 1)

	do := func(method, target, token, body string, header http.Header) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		for name, values := range header {
			req.Header[name] = values
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	t.Run("post is proxied ahead of the built-in route", func(t *testing.T) {
		rec := do(http.MethodPost, "/_matrix/client/v1/rtc/livekit/get_token?x=1", "alice_token", `{"a":1}`, http.Header{
			"Content-Type": {"application/json"}, "Accept-Language": {"de"}, "X-Not-Forwarded": {"1"}, "Cookie": {"c=1"},
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		require.JSONEq(t, `{"jwt":"token"}`, rec.Body.String())
		require.False(t, builtIn)
		seen := fake.last(t)
		require.Equal(t, http.MethodPost, seen.method)
		require.Equal(t, "/_matrix/client/v1/rtc/livekit/get_token?x=1", seen.uri)
		require.JSONEq(t, `{"a":1}`, seen.body)
		require.Equal(t, "Bearer hs_token", seen.header.Get("Authorization"))
		require.Equal(t, "@alice:local", seen.header.Get(appserviceproxy.UserIDHeader))
		require.Equal(t, "application/json", seen.header.Get("Content-Type"))
		require.Equal(t, "de", seen.header.Get("Accept-Language"))
		require.Empty(t, seen.header.Get("X-Not-Forwarded"))
		require.Empty(t, seen.header.Get("Cookie"))

		require.Equal(t, "kept", rec.Header().Get("X-Custom"))
		require.Empty(t, rec.Header().Get("X-Hop"))
		require.Empty(t, rec.Header().Get("Keep-Alive"))
		require.Empty(t, rec.Header().Get("Connection"))
		require.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
	})

	t.Run("unstable prefix and root path", func(t *testing.T) {
		rec := do(http.MethodGet, "/_matrix/client/unstable/io.element.msc4195/rtc/livekit", "alice_token", "", nil)
		require.Equal(t, http.StatusCreated, rec.Code)
		require.Equal(t, "/_matrix/client/unstable/io.element.msc4195/rtc/livekit", fake.last(t).uri)
	})

	t.Run("access token in the query is stripped", func(t *testing.T) {
		rec := do(http.MethodGet, "/_matrix/client/v1/rtc/livekit/x?access_token=alice_token&keep=yes", "", "", nil)
		require.Equal(t, http.StatusCreated, rec.Code)
		require.Equal(t, "/_matrix/client/v1/rtc/livekit/x?keep=yes", fake.last(t).uri)
	})

	t.Run("guests may use rtc/livekit", func(t *testing.T) {
		rec := do(http.MethodGet, "/_matrix/client/v1/rtc/livekit/x", "guest_token", "", nil)
		require.Equal(t, http.StatusCreated, rec.Code)
		require.Equal(t, "@guest:local", fake.last(t).header.Get(appserviceproxy.UserIDHeader))
	})

	t.Run("unauthenticated is refused", func(t *testing.T) {
		before := fake.count()
		rec := do(http.MethodGet, "/_matrix/client/v1/rtc/livekit/x", "", "", nil)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
		rec = do(http.MethodGet, "/_matrix/client/v1/rtc/livekit/x", "bad", "", nil)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
		require.Equal(t, before, fake.count())
	})

	t.Run("dot segments are refused", func(t *testing.T) {
		before := fake.count()
		for _, path := range []string{
			"/_matrix/client/v1/rtc/livekit/../../v3/sync",
			"/_matrix/client/v1/rtc/livekit/%2e%2e/%2e%2e/v3/sync",
		} {
			rec := do(http.MethodGet, path, "alice_token", "", nil)
			require.Equal(t, http.StatusBadRequest, rec.Code, path)
		}
		require.Equal(t, before, fake.count())
	})

	t.Run("other paths are left to the routes", func(t *testing.T) {
		before := fake.count()
		require.Equal(t, http.StatusTeapot, do(http.MethodGet, "/_matrix/client/v3/sync", "alice_token", "", nil).Code)
		require.Equal(t, http.StatusNotFound, do(http.MethodGet, "/_matrix/client/v1/rtc/livekitfoo", "alice_token", "", nil).Code)
		require.Equal(t, http.StatusNotFound, do(http.MethodGet, "/_matrix/client/rtc/livekit/x", "alice_token", "", nil).Code)
		require.Equal(t, before, fake.count())
	})

	t.Run("oversized body is refused", func(t *testing.T) {
		before := fake.count()
		rec := do(http.MethodPost, "/_matrix/client/v1/rtc/livekit/x", "alice_token", strings.Repeat("a", 10<<20+1), nil)
		require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
		require.Equal(t, before, fake.count())
	})

	t.Run("preflight is answered", func(t *testing.T) {
		before := fake.count()
		rec := do(http.MethodOptions, "/_matrix/client/v1/rtc/livekit/get_token", "", "", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
		require.Equal(t, before, fake.count())
	})
}

func TestClientProxyLegacyAuthAndUnreachable(t *testing.T) {
	fake := &fakeAppservice{}
	ases := appservices(t, fake)
	userAPI := &tokenUserAPI{devices: map[string]userapi.Device{"t": {UserID: "@alice:local", AccountType: userapi.AccountTypeUser}}}
	router := httputil.NewRouter(httputil.PublicClientPathPrefix)
	appserviceproxy.RegisterClient(router, ases, userAPI, httputil.NewRateLimits(&config.RateLimiting{}), true)

	req := httptest.NewRequest(http.MethodGet, "/_matrix/client/v1/rtc/livekit/x", nil)
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)
	require.Equal(t, "/_matrix/client/v1/rtc/livekit/x?access_token=hs_token", fake.last(t).uri)

	ases[0].ProxyURL = "http://127.0.0.1:1"
	router = httputil.NewRouter(httputil.PublicClientPathPrefix)
	appserviceproxy.RegisterClient(router, ases, userAPI, httputil.NewRateLimits(&config.RateLimiting{}), false)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadGateway, rec.Code)
}

// keyVerifier checks signatures against one server's key.
type keyVerifier struct {
	serverName spec.ServerName
	keyID      gomatrixserverlib.KeyID
	public     ed25519.PublicKey
}

func (v keyVerifier) VerifyJSONs(_ context.Context, requests []gomatrixserverlib.VerifyJSONRequest) ([]gomatrixserverlib.VerifyJSONResult, error) {
	results := make([]gomatrixserverlib.VerifyJSONResult, len(requests))
	for i, req := range requests {
		if req.ServerName != v.serverName {
			results[i].Error = gomatrixserverlib.VerifyJSON(string(req.ServerName), v.keyID, nil, req.Message)
			continue
		}
		results[i].Error = gomatrixserverlib.VerifyJSON(string(req.ServerName), v.keyID, v.public, req.Message)
	}
	return results, nil
}

func TestFederationProxy(t *testing.T) {
	fake := &fakeAppservice{}
	ases := appservices(t, fake)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	const keyID = gomatrixserverlib.KeyID("ed25519:test")
	keys := keyVerifier{serverName: "remote", keyID: keyID, public: public}

	router := httputil.NewRouter(httputil.PublicFederationPathPrefix)
	router.Handle("/v1/rtc/livekit/get_token", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})).Methods(http.MethodPost)
	woken := make(chan spec.ServerName, 10)
	isLocal := func(s spec.ServerName) bool { return s == "local" }
	claimed := appserviceproxy.RegisterFederation(router, ases, "local", isLocal, keys,
		func(_ context.Context, origin spec.ServerName) { woken <- origin }, false)
	require.Len(t, claimed, 1)

	signed := func(t *testing.T, method, uri string, content any) *http.Request {
		t.Helper()
		fedReq := fclient.NewFederationRequest(method, "remote", "local", uri)
		if content != nil {
			require.NoError(t, fedReq.SetContent(content))
		}
		require.NoError(t, fedReq.Sign("remote", keyID, private))
		httpReq, err := fedReq.HTTPRequest()
		require.NoError(t, err)
		httpReq.RequestURI = uri
		if httpReq.Body == nil {
			httpReq.Body = http.NoBody
		}
		return httpReq
	}
	serve := func(req *http.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	t.Run("signed post is proxied", func(t *testing.T) {
		rec := serve(signed(t, http.MethodPost, "/_matrix/federation/v1/rtc/livekit/get_token", map[string]string{"room_id": "!r:local"}))
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		seen := fake.last(t)
		require.Equal(t, "/_matrix/federation/v1/rtc/livekit/get_token", seen.uri)
		require.JSONEq(t, `{"room_id":"!r:local"}`, seen.body)
		require.Equal(t, "remote", seen.header.Get(appserviceproxy.OriginHeader))
		require.Equal(t, "Bearer hs_token", seen.header.Get("Authorization"))
		select {
		case origin := <-woken:
			require.Equal(t, spec.ServerName("remote"), origin)
		case <-time.After(5 * time.Second):
			t.Fatal("the origin was not woken up")
		}
	})

	t.Run("signed get on the unstable prefix is proxied", func(t *testing.T) {
		rec := serve(signed(t, http.MethodGet, "/_matrix/federation/unstable/io.element.msc4195/rtc/livekit/x?a=b", nil))
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		require.Equal(t, "/_matrix/federation/unstable/io.element.msc4195/rtc/livekit/x?a=b", fake.last(t).uri)
	})

	t.Run("unsigned is refused", func(t *testing.T) {
		before := fake.count()
		req := httptest.NewRequest(http.MethodGet, "/_matrix/federation/v1/rtc/livekit/x", nil)
		require.Equal(t, http.StatusUnauthorized, serve(req).Code)
		require.Equal(t, before, fake.count())
	})

	t.Run("bad signature is refused", func(t *testing.T) {
		before := fake.count()
		req := signed(t, http.MethodGet, "/_matrix/federation/v1/rtc/livekit/x", nil)
		req.Header.Set("Authorization", strings.Replace(req.Header.Get("Authorization"), `sig="`, `sig="AAAA`, 1))
		require.Equal(t, http.StatusUnauthorized, serve(req).Code)
		require.Equal(t, before, fake.count())
	})

	t.Run("options is left to the routes", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/_matrix/federation/v1/rtc/livekit/get_token", nil)
		require.NotEqual(t, http.StatusUnauthorized, serve(req).Code)
	})

	t.Run("dot segment is refused", func(t *testing.T) {
		before := fake.count()
		req := httptest.NewRequest(http.MethodGet, "/_matrix/federation/v1/rtc/livekit/%2e%2e/x", nil)
		require.Equal(t, http.StatusBadRequest, serve(req).Code)
		require.Equal(t, before, fake.count())
	})

	t.Run("rate limited per origin", func(t *testing.T) {
		limited := false
		for range 200 {
			rec := serve(signed(t, http.MethodGet, "/_matrix/federation/v1/rtc/livekit/x", nil))
			if rec.Code == http.StatusTooManyRequests {
				var body map[string]any
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
				require.Equal(t, "M_LIMIT_EXCEEDED", body["errcode"])
				limited = true
				break
			}
		}
		require.True(t, limited)
	})
}
