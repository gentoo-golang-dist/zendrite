// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package clientapi

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/httputil"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
	"codefloe.com/pat-s/zendrite/userapi"
	uapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// rendezvousUserAPI accepts fixed access tokens, standing in for the
// MSC3861 introspection that QR code login needs, and stores rendezvous
// sessions in the real user API.
type rendezvousUserAPI struct {
	uapi.ClientUserAPI
	devices map[string]*uapi.Device
}

func (u *rendezvousUserAPI) QueryAccessToken(_ context.Context, req *uapi.QueryAccessTokenRequest, res *uapi.QueryAccessTokenResponse) error {
	res.Device = u.devices[req.AccessToken]
	return nil
}

const (
	msc4388Path = "/_matrix/client/unstable/io.element.msc4388/rendezvous"
	msc4108Path = "/_matrix/client/unstable/org.matrix.msc4108/rendezvous"
)

type rendezvousServer struct {
	t       *testing.T
	cfg     *config.Zendrite
	routers httputil.Routers
}

func (s *rendezvousServer) do(req *http.Request) *httptest.ResponseRecorder {
	s.t.Helper()
	rec := httptest.NewRecorder()
	s.routers.Client.ServeHTTP(rec, req)
	return rec
}

func (s *rendezvousServer) json(method, path, token, body string, headers ...string) (int, gjson.Result) {
	s.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := s.do(req)
	return rec.Code, gjson.ParseBytes(rec.Body.Bytes())
}

// newRendezvousServer starts the client API with MSC3861 on, after mutate
// has adjusted the configuration.
func newRendezvousServer(t *testing.T, dbType test.DBType, mutate func(*config.Zendrite)) *rendezvousServer {
	cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
	t.Cleanup(closeDB)
	cfg.MSCs.MSCs = append(cfg.MSCs.MSCs, "msc3861")
	cfg.MSCs.MSC3861.Issuer = "https://auth.example.com/"
	cfg.MSCs.MSC3861.PublicBaseURL = "https://matrix.example.com"
	cfg.MSCs.Rendezvous.RateLimiting.Enabled = false
	cfg.MSCs.Rendezvous.CreateRateLimiting.Enabled = false
	if mutate != nil {
		mutate(cfg)
	}
	routers := httputil.NewRouters()
	cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
	caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
	natsInstance := jetstream.NATSInstance{}
	rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
	rsAPI.SetFederationAPI(nil, nil)
	realUserAPI := userapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, rsAPI, nil, caching.DisableMetrics, testIsBlacklistedOrBackingOff)
	userAPI := &rendezvousUserAPI{ClientUserAPI: realUserAPI, devices: map[string]*uapi.Device{
		"alice_token": {UserID: "@alice:test", ID: "ALICEDEVICE", AccountType: uapi.AccountTypeUser},
		"guest_token": {UserID: "@guest:test", ID: "GUESTDEVICE", AccountType: uapi.AccountTypeGuest},
	}}
	AddPublicRoutes(processCtx, routers, cfg, &natsInstance, nil, rsAPI, nil, nil, nil, userAPI, nil, nil, nil, caching.DisableMetrics)
	return &rendezvousServer{t: t, cfg: cfg, routers: routers}
}

// TestRendezvousMSC4388 covers the MSC4388 rendezvous API behind QR code
// login (MSC4108).
func TestRendezvousMSC4388(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		s := newRendezvousServer(t, dbType, nil)
		create := func(token, data string) (int, gjson.Result) {
			t.Helper()
			return s.json(http.MethodPost, msc4388Path, token, `{"data":`+gjson.Parse(`"`+data+`"`).Raw+`}`)
		}

		t.Run("authenticated mode", func(t *testing.T) {
			code, res := s.json(http.MethodGet, msc4388Path, "", "")
			require.Equal(t, http.StatusOK, code, res.Raw)
			require.False(t, res.Get("create_available").Bool())
			code, res = s.json(http.MethodGet, msc4388Path, "alice_token", "")
			require.Equal(t, http.StatusOK, code, res.Raw)
			require.True(t, res.Get("create_available").Bool())
			code, res = s.json(http.MethodGet, msc4388Path, "guest_token", "")
			require.Equal(t, http.StatusOK, code, res.Raw)
			require.False(t, res.Get("create_available").Bool())

			// A bad token is an error, not an anonymous request.
			code, res = s.json(http.MethodGet, msc4388Path, "bad_token", "")
			require.Equal(t, http.StatusUnauthorized, code, res.Raw)
			require.Equal(t, "M_UNKNOWN_TOKEN", res.Get("errcode").Str)
			code, res = create("bad_token", "")
			require.Equal(t, http.StatusUnauthorized, code, res.Raw)

			code, res = create("", "")
			require.Equal(t, http.StatusForbidden, code, res.Raw)
			require.Equal(t, "M_FORBIDDEN", res.Get("errcode").Str)
			code, res = create("guest_token", "")
			require.Equal(t, http.StatusForbidden, code, res.Raw)
			code, res = create("alice_token", "")
			require.Equal(t, http.StatusOK, code, res.Raw)
		})

		t.Run("open mode", func(t *testing.T) {
			s.cfg.MSCs.Rendezvous.MSC4388Mode = config.RendezvousModeOpen
			defer func() { s.cfg.MSCs.Rendezvous.MSC4388Mode = "" }()
			code, res := s.json(http.MethodGet, msc4388Path, "", "")
			require.Equal(t, http.StatusOK, code, res.Raw)
			require.True(t, res.Get("create_available").Bool())
			code, res = create("", "anonymous")
			require.Equal(t, http.StatusOK, code, res.Raw)
			// Even in open mode a bad token is refused.
			code, res = create("bad_token", "")
			require.Equal(t, http.StatusUnauthorized, code, res.Raw)
		})

		t.Run("exchange", func(t *testing.T) {
			code, res := create("alice_token", "hello from A")
			require.Equal(t, http.StatusOK, code, res.Raw)
			id := res.Get("id").Str
			require.Regexp(t, `^[0-9A-Za-z._~-]{32}$`, id)
			require.Equal(t, "1", res.Get("sequence_token").Str)
			require.InDelta(t, s.cfg.MSCs.Rendezvous.SessionLifetime.Milliseconds(), res.Get("expires_in_ms").Int(), 5000)
			session := msc4388Path + "/" + id

			// The other device reads without a token.
			code, res = s.json(http.MethodGet, session, "", "")
			require.Equal(t, http.StatusOK, code, res.Raw)
			require.Equal(t, "hello from A", res.Get("data").Str)
			require.Equal(t, "1", res.Get("sequence_token").Str)
			require.Positive(t, res.Get("expires_in_ms").Int())

			code, res = s.json(http.MethodPut, session+"/txnB1", "", `{"sequence_token":"1","data":"hello from B"}`)
			require.Equal(t, http.StatusOK, code, res.Raw)
			require.Equal(t, "2", res.Get("sequence_token").Str)

			// A stale sequence token is a concurrent write.
			code, res = s.json(http.MethodPut, session+"/txnA1", "", `{"sequence_token":"1","data":"late"}`)
			require.Equal(t, http.StatusConflict, code, res.Raw)
			require.Equal(t, "IO_ELEMENT_MSC4388_CONCURRENT_WRITE", res.Get("errcode").Str)
			code, res = s.json(http.MethodPut, session+"/txnA2", "", `{"sequence_token":"not a number","data":"x"}`)
			require.Equal(t, http.StatusConflict, code, res.Raw)

			// A retry gets the recorded answer, even with a body that would
			// otherwise fail, and changes nothing.
			for _, body := range []string{`{"sequence_token":"1","data":"hello from B"}`, `not json`, `{"sequence_token":"9","data":"` + strings.Repeat("x", 5000) + `"}`} {
				code, res = s.json(http.MethodPut, session+"/txnB1", "", body)
				require.Equal(t, http.StatusOK, code, res.Raw)
				require.Equal(t, "2", res.Get("sequence_token").Str)
			}
			code, res = s.json(http.MethodGet, session, "", "")
			require.Equal(t, http.StatusOK, code, res.Raw)
			require.Equal(t, "hello from B", res.Get("data").Str)
			require.Equal(t, "2", res.Get("sequence_token").Str)

			// Writing the same data again still changes the token.
			code, res = s.json(http.MethodPut, session+"/txnA3", "", `{"sequence_token":"2","data":"hello from B"}`)
			require.Equal(t, http.StatusOK, code, res.Raw)
			require.Equal(t, "3", res.Get("sequence_token").Str)

			code, res = s.json(http.MethodDelete, session, "", "")
			require.Equal(t, http.StatusOK, code, res.Raw)
			require.JSONEq(t, `{}`, res.Raw)
			for _, method := range []string{http.MethodGet, http.MethodDelete} {
				code, res = s.json(method, session, "", "")
				require.Equal(t, http.StatusNotFound, code, res.Raw)
				require.Equal(t, "M_NOT_FOUND", res.Get("errcode").Str)
			}
			code, res = s.json(http.MethodPut, session+"/txnB1", "", `{"sequence_token":"3","data":"x"}`)
			require.Equal(t, http.StatusNotFound, code, res.Raw)
		})

		t.Run("validation", func(t *testing.T) {
			code, res := s.json(http.MethodPost, msc4388Path, "alice_token", `not json`)
			require.Equal(t, http.StatusBadRequest, code, res.Raw)
			require.Equal(t, "M_NOT_JSON", res.Get("errcode").Str)
			code, res = s.json(http.MethodPost, msc4388Path, "alice_token", `{"data":1}`)
			require.Equal(t, http.StatusBadRequest, code, res.Raw)
			require.Equal(t, "M_BAD_JSON", res.Get("errcode").Str)
			code, res = create("alice_token", strings.Repeat("x", 4097))
			require.Equal(t, http.StatusRequestEntityTooLarge, code, res.Raw)
			require.Equal(t, "M_TOO_LARGE", res.Get("errcode").Str)
			code, res = create("alice_token", strings.Repeat("x", 4096))
			require.Equal(t, http.StatusOK, code, res.Raw)
			session := msc4388Path + "/" + res.Get("id").Str

			code, res = s.json(http.MethodPut, session+"/txn1", "", `{"sequence_token":"1","data":"`+strings.Repeat("x", 4097)+`"}`)
			require.Equal(t, http.StatusRequestEntityTooLarge, code, res.Raw)
			code, res = s.json(http.MethodPut, session+"/txn1", "", `{"data":"x"}`)
			require.Equal(t, http.StatusBadRequest, code, res.Raw)
			require.Equal(t, "M_BAD_JSON", res.Get("errcode").Str)
			code, res = s.json(http.MethodPut, session+"/txn1", "", `[]`)
			require.Equal(t, http.StatusBadRequest, code, res.Raw)
			require.Equal(t, "M_NOT_JSON", res.Get("errcode").Str)
			code, res = s.json(http.MethodPut, session+"/bad%20txn", "", `{"sequence_token":"1","data":"x"}`)
			require.Equal(t, http.StatusBadRequest, code, res.Raw)
			require.Equal(t, "M_INVALID_PARAM", res.Get("errcode").Str)
			code, res = s.json(http.MethodPut, session+"/"+strings.Repeat("t", 256), "", `{"sequence_token":"1","data":"x"}`)
			require.Equal(t, http.StatusBadRequest, code, res.Raw)

			code, res = s.json(http.MethodGet, msc4388Path+"/unknown", "", "")
			require.Equal(t, http.StatusNotFound, code, res.Raw)
			require.Equal(t, "M_NOT_FOUND", res.Get("errcode").Str)
		})

		t.Run("browser navigation is refused", func(t *testing.T) {
			code, res := create("alice_token", "<script>")
			require.Equal(t, http.StatusOK, code, res.Raw)
			session := msc4388Path + "/" + res.Get("id").Str
			for _, header := range [][2]string{
				{"Sec-Fetch-Dest", "document"},
				{"Sec-Fetch-Mode", "navigate"},
				{"Sec-Fetch-User", "?1"},
				{"Sec-Fetch-Site", "none"},
			} {
				code, res = s.json(http.MethodGet, session, "", "", header[0], header[1])
				require.Equal(t, http.StatusForbidden, code, "%v: %s", header, res.Raw)
				require.Equal(t, "M_FORBIDDEN", res.Get("errcode").Str)
			}
			// A fetch from a web client is fine.
			code, res = s.json(http.MethodGet, session, "", "", "Sec-Fetch-Dest", "empty")
			require.Equal(t, http.StatusOK, code, res.Raw)
		})

		t.Run("CORS", func(t *testing.T) {
			for _, path := range []string{msc4388Path, msc4388Path + "/id", msc4388Path + "/id/txn"} {
				req := httptest.NewRequest(http.MethodOptions, path, nil)
				req.Header.Set("Access-Control-Request-Method", http.MethodPut)
				rec := s.do(req)
				require.Equal(t, http.StatusOK, rec.Code, path)
				require.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"), path)
				require.Contains(t, rec.Header().Get("Access-Control-Allow-Methods"), "PUT", path)
				require.Contains(t, rec.Header().Get("Access-Control-Allow-Methods"), "DELETE", path)
				require.Contains(t, rec.Header().Get("Access-Control-Allow-Headers"), "Authorization", path)
			}
		})
	})
}

// TestRendezvousExpiryAndLimits covers session expiry, the session limit and
// rate limiting.
func TestRendezvousExpiryAndLimits(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		s := newRendezvousServer(t, dbType, func(cfg *config.Zendrite) {
			cfg.MSCs.Rendezvous.MSC4388Mode = config.RendezvousModeOpen
			cfg.MSCs.Rendezvous.SessionLifetime = 500 * time.Millisecond
			cfg.MSCs.Rendezvous.MaxSessions = 3
		})

		code, res := s.json(http.MethodPost, msc4388Path, "", `{"data":"x"}`)
		require.Equal(t, http.StatusOK, code, res.Raw)
		session := msc4388Path + "/" + res.Get("id").Str
		require.LessOrEqual(t, res.Get("expires_in_ms").Int(), int64(500))
		code, res = s.json(http.MethodPost, msc4388Path, "", `{"data":"y"}`)
		require.Equal(t, http.StatusOK, code, res.Raw)
		rec := s.do(func() *http.Request {
			req := httptest.NewRequest(http.MethodPost, msc4108Path, strings.NewReader("z"))
			req.Header.Set("Content-Type", "text/plain")
			return req
		}())
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		// Anonymous sessions of both APIs share the limit.
		code, res = s.json(http.MethodPost, msc4388Path, "", `{"data":"too many"}`)
		require.Equal(t, http.StatusTooManyRequests, code, res.Raw)
		require.Equal(t, "M_LIMIT_EXCEEDED", res.Get("errcode").Str)
		// A guest counts as anonymous.
		code, res = s.json(http.MethodPost, msc4388Path, "guest_token", `{"data":"guest"}`)
		require.Equal(t, http.StatusTooManyRequests, code, res.Raw)
		// A signed-in user is not locked out by anonymous sessions, but has a
		// limit of their own.
		for range 10 {
			code, res = s.json(http.MethodPost, msc4388Path, "alice_token", `{"data":"signed in"}`)
			require.Equal(t, http.StatusOK, code, res.Raw)
		}
		code, res = s.json(http.MethodPost, msc4388Path, "alice_token", `{"data":"signed in"}`)
		require.Equal(t, http.StatusTooManyRequests, code, res.Raw)

		time.Sleep(600 * time.Millisecond)
		code, res = s.json(http.MethodGet, session, "", "")
		require.Equal(t, http.StatusNotFound, code, res.Raw)
		code, res = s.json(http.MethodPut, session+"/txn", "", `{"sequence_token":"1","data":"x"}`)
		require.Equal(t, http.StatusNotFound, code, res.Raw)
		code, res = s.json(http.MethodDelete, session, "", "")
		require.Equal(t, http.StatusNotFound, code, res.Raw)
		// Expired sessions no longer count towards the limit.
		code, res = s.json(http.MethodPost, msc4388Path, "", `{"data":"x"}`)
		require.Equal(t, http.StatusOK, code, res.Raw)
	})

	t.Run("rate limits", func(t *testing.T) {
		s := newRendezvousServer(t, test.DBTypeSQLite, func(cfg *config.Zendrite) {
			cfg.MSCs.Rendezvous.MSC4388Mode = config.RendezvousModeOpen
			cfg.MSCs.Rendezvous.RateLimiting = config.RateLimiting{Enabled: true, Threshold: 5, CooloffMS: 60000}
			cfg.MSCs.Rendezvous.CreateRateLimiting = config.RateLimiting{Enabled: true, Threshold: 2, CooloffMS: 60000}
		})
		for range 2 {
			code, res := s.json(http.MethodPost, msc4388Path, "", `{"data":"x"}`)
			require.Equal(t, http.StatusOK, code, res.Raw)
		}
		code, res := s.json(http.MethodPost, msc4388Path, "", `{"data":"x"}`)
		require.Equal(t, http.StatusTooManyRequests, code, res.Raw)
		require.Equal(t, "M_LIMIT_EXCEEDED", res.Get("errcode").Str)
		code, res = s.json(http.MethodPost, msc4108Path, "", "x", "Content-Type", "text/plain")
		require.Equal(t, http.StatusTooManyRequests, code, res.Raw)
		// Reads share the general limit, which four requests have used.
		code, res = s.json(http.MethodGet, msc4388Path+"/unknown", "", "")
		require.Equal(t, http.StatusNotFound, code, res.Raw)
		code, res = s.json(http.MethodGet, msc4388Path+"/unknown", "", "")
		require.Equal(t, http.StatusTooManyRequests, code, res.Raw)
	})
}

// TestRendezvousMSC4108 covers the 2024 MSC4108 rendezvous API that Element
// X and matrix-rust-sdk still create their sessions through.
func TestRendezvousMSC4108(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		s := newRendezvousServer(t, dbType, nil)
		request := func(method, path, body string, headers ...string) *httptest.ResponseRecorder {
			t.Helper()
			var reader io.Reader
			if body != "" || method == http.MethodPost || method == http.MethodPut {
				reader = bytes.NewBufferString(body)
			}
			req := httptest.NewRequest(method, path, reader)
			for i := 0; i+1 < len(headers); i += 2 {
				req.Header.Set(headers[i], headers[i+1])
			}
			return s.do(req)
		}
		requireSessionHeaders := func(rec *httptest.ResponseRecorder, etag string) {
			t.Helper()
			require.Equal(t, etag, rec.Header().Get("ETag"))
			_, err := http.ParseTime(rec.Header().Get("Expires"))
			require.NoError(t, err)
			_, err = http.ParseTime(rec.Header().Get("Last-Modified"))
			require.NoError(t, err)
			require.Contains(t, rec.Header().Get("Cache-Control"), "no-store")
		}

		// The new device creates the session without an access token, with an
		// empty payload.
		rec := request(http.MethodPost, msc4108Path, "", "Content-Type", "text/plain")
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		requireSessionHeaders(rec, `"1"`)
		url := gjson.GetBytes(rec.Body.Bytes(), "url").Str
		require.True(t, strings.HasPrefix(url, "https://matrix.example.com"+msc4108Path+"/"), url)
		session := strings.TrimPrefix(url, "https://matrix.example.com")

		// The scanning device reads the initial, empty payload.
		rec = request(http.MethodGet, session, "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		requireSessionHeaders(rec, `"1"`)
		require.Equal(t, "text/plain", rec.Header().Get("Content-Type"))
		require.Empty(t, rec.Body.String())

		rec = request(http.MethodPut, session, "hello from B", "Content-Type", "text/plain", "If-Match", `"1"`)
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		requireSessionHeaders(rec, `"2"`)

		// Polling with the latest ETag is answered with 304 until it changes.
		rec = request(http.MethodGet, session, "", "If-None-Match", `"2"`)
		require.Equal(t, http.StatusNotModified, rec.Code)
		requireSessionHeaders(rec, `"2"`)
		rec = request(http.MethodGet, session, "", "If-None-Match", `"1"`)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "hello from B", rec.Body.String())

		// A stale ETag is a concurrent write.
		rec = request(http.MethodPut, session, "late", "Content-Type", "text/plain", "If-Match", `"1"`)
		require.Equal(t, http.StatusPreconditionFailed, rec.Code)
		requireSessionHeaders(rec, `"2"`)
		require.Equal(t, "M_UNKNOWN", gjson.GetBytes(rec.Body.Bytes(), "errcode").Str)
		require.Equal(t, "M_CONCURRENT_WRITE", gjson.GetBytes(rec.Body.Bytes(), "org\\.matrix\\.msc4108\\.errcode").Str)

		rec = request(http.MethodPut, session, "x", "Content-Type", "text/plain")
		require.Equal(t, http.StatusBadRequest, rec.Code)
		rec = request(http.MethodPut, session, "x", "Content-Type", "application/json", "If-Match", `"2"`)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		rec = request(http.MethodPut, session, strings.Repeat("x", 4097), "Content-Type", "text/plain", "If-Match", `"2"`)
		require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
		rec = request(http.MethodGet, session, "", "Sec-Fetch-Mode", "navigate")
		require.Equal(t, http.StatusForbidden, rec.Code)

		// An MSC4108 session is not visible through MSC4388, and vice versa.
		id := strings.TrimPrefix(session, msc4108Path+"/")
		code, res := s.json(http.MethodGet, msc4388Path+"/"+id, "", "")
		require.Equal(t, http.StatusNotFound, code, res.Raw)

		// CORS lets browsers send the precondition headers and read the ETag.
		req := httptest.NewRequest(http.MethodOptions, session, nil)
		req.Header.Set("Access-Control-Request-Method", http.MethodPut)
		rec = s.do(req)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Contains(t, rec.Header().Get("Access-Control-Allow-Headers"), "If-Match")
		require.Contains(t, rec.Header().Get("Access-Control-Allow-Headers"), "If-None-Match")
		require.Contains(t, rec.Header().Get("Access-Control-Expose-Headers"), "ETag")
		rec = request(http.MethodGet, session, "")
		require.Contains(t, rec.Header().Get("Access-Control-Expose-Headers"), "ETag")

		rec = request(http.MethodDelete, session, "")
		require.Equal(t, http.StatusNoContent, rec.Code)
		rec = request(http.MethodGet, session, "")
		require.Equal(t, http.StatusNotFound, rec.Code)
		require.Equal(t, "M_NOT_FOUND", gjson.GetBytes(rec.Body.Bytes(), "errcode").Str)
		rec = request(http.MethodDelete, session, "")
		require.Equal(t, http.StatusNotFound, rec.Code)
	})
}

// TestRendezvousDisabled checks that the rendezvous APIs and their flag are
// absent without MSC3861 or when turned off.
func TestRendezvousDisabled(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config.Zendrite)
	}{
		{"without MSC3861", func(cfg *config.Zendrite) { cfg.MSCs.MSCs = nil }},
		{"turned off", func(cfg *config.Zendrite) {
			cfg.MSCs.Rendezvous.MSC4388Mode = config.RendezvousModeOff
			cfg.MSCs.Rendezvous.MSC4108Enabled = new(false)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRendezvousServer(t, test.DBTypeSQLite, tc.mutate)
			for _, path := range []string{msc4388Path, msc4108Path} {
				code, res := s.json(http.MethodPost, path, "alice_token", `{"data":""}`, "Content-Type", "text/plain")
				require.Equal(t, http.StatusNotFound, code, res.Raw)
				require.Equal(t, "M_UNRECOGNIZED", res.Get("errcode").Str)
			}
			code, res := s.json(http.MethodGet, msc4388Path, "alice_token", "")
			require.Equal(t, http.StatusNotFound, code, res.Raw)
			code, res = s.json(http.MethodGet, "/_matrix/client/versions", "", "")
			require.Equal(t, http.StatusOK, code, res.Raw)
			require.False(t, res.Get(`unstable_features.org\.matrix\.msc4108`).Bool())
		})
	}
}
