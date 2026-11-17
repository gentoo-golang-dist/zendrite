// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/internal/httputil"
	"codefloe.com/pat-s/zendrite/setup/config"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

type userTokenAPI struct{}

func (userTokenAPI) QueryAccessToken(_ context.Context, _ *userapi.QueryAccessTokenRequest, res *userapi.QueryAccessTokenResponse) error {
	res.Device = &userapi.Device{UserID: "@alice:test", ID: "DEVICE", AccountType: userapi.AccountTypeUser}
	return nil
}

// TestURLPreviewDisabled checks that, with URL previews disabled, both
// preview_url routes answer 403 M_FORBIDDEN, as MSC4452 asks, rather than 404.
func TestURLPreviewDisabled(t *testing.T) {
	cfg := &config.Zendrite{}
	cfg.MediaAPI.Matrix = &cfg.Global
	cfg.ClientAPI.Matrix = &cfg.Global
	routers := httputil.NewRouters()
	Setup(routers, cfg, nil, nil, userTokenAPI{}, nil, nil, nil)

	for _, tc := range []struct {
		router http.Handler
		path   string
	}{
		{routers.Client, "/_matrix/client/v1/media/preview_url?url=https://example.com"},
		{routers.Media, "/_matrix/media/v3/preview_url?url=https://example.com"},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.Header.Set("Authorization", "Bearer token")
		rec := httptest.NewRecorder()
		tc.router.ServeHTTP(rec, req)
		require.Equal(t, http.StatusForbidden, rec.Code, tc.path)
		require.Equal(t, "M_FORBIDDEN", gjson.GetBytes(rec.Body.Bytes(), "errcode").Str, tc.path)
	}

	// An access token is still required.
	req := httptest.NewRequest(http.MethodGet, "/_matrix/client/v1/media/preview_url?url=https://example.com", nil)
	rec := httptest.NewRecorder()
	routers.Client.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}
