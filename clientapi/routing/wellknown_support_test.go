// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/httputil"
	"codefloe.com/pat-s/zendrite/setup/config"
)

func serveWellKnownSupport(t *testing.T, support config.Support) *httptest.ResponseRecorder {
	t.Helper()
	var cfg config.Zendrite
	cfg.Defaults(config.DefaultOpts{})
	cfg.Global.Support = support

	routers := httputil.NewRouters()
	Setup(routers, &cfg, nil, nil, &discoveryUserAPI{}, nil, nil, nil, nil, nil, nil, nil, nil, caching.DisableMetrics)

	req := httptest.NewRequest(http.MethodGet, "/.well-known/matrix/support", nil)
	rec := httptest.NewRecorder()
	routers.WellKnown.ServeHTTP(rec, req)
	return rec
}

// TestWellKnownSupport checks the MSC1929 response shape: empty fields are
// left out and the contact keys match the spec.
func TestWellKnownSupport(t *testing.T) {
	t.Parallel()

	rec := serveWellKnownSupport(t, config.Support{
		Contacts: []config.SupportContact{
			{MatrixID: "@admin:example.org", EmailAddress: "admin@example.org", Role: "m.role.admin"},
			{EmailAddress: "security@example.org", Role: "m.role.security"},
		},
		SupportPage: "https://example.org/support.html",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))

	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, map[string]any{
		"contacts": []any{
			map[string]any{"matrix_id": "@admin:example.org", "email_address": "admin@example.org", "role": "m.role.admin"},
			map[string]any{"email_address": "security@example.org", "role": "m.role.security"},
		},
		"support_page": "https://example.org/support.html",
	}, got)
}

func TestWellKnownSupportPageOnly(t *testing.T) {
	t.Parallel()

	rec := serveWellKnownSupport(t, config.Support{SupportPage: "https://example.org/help"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"support_page":"https://example.org/help"}`, rec.Body.String())
}

// TestWellKnownSupportUnconfigured returns 404, which tells clients that no
// support information is available.
func TestWellKnownSupportUnconfigured(t *testing.T) {
	t.Parallel()

	rec := serveWellKnownSupport(t, config.Support{})
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

// TestWellKnownClientCORS checks that /.well-known/matrix/client answers web
// clients, including their preflight requests: MSC4402 lets clients follow
// redirects to it, and asks every hop to send CORS headers.
func TestWellKnownClientCORS(t *testing.T) {
	t.Parallel()

	var cfg config.Zendrite
	cfg.Defaults(config.DefaultOpts{})
	cfg.Global.WellKnownClientName = "https://matrix.example.com"

	routers := httputil.NewRouters()
	Setup(routers, &cfg, nil, nil, &discoveryUserAPI{}, nil, nil, nil, nil, nil, nil, nil, nil, caching.DisableMetrics)

	for _, method := range []string{http.MethodGet, http.MethodOptions} {
		req := httptest.NewRequest(method, "/.well-known/matrix/client", nil)
		req.Header.Set("Origin", "https://app.example.org")
		if method == http.MethodOptions {
			req.Header.Set("Access-Control-Request-Method", http.MethodGet)
		}
		rec := httptest.NewRecorder()
		routers.WellKnown.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, method)
		assert.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"), method)
		assert.Contains(t, rec.Header().Get("Access-Control-Allow-Methods"), http.MethodGet, method)
		assert.Contains(t, rec.Header().Get("Access-Control-Allow-Headers"), "Authorization", method)
		if method == http.MethodGet {
			assert.JSONEq(t, `{"m.homeserver":{"base_url":"https://matrix.example.com"}}`, rec.Body.String())
		}
	}
}
