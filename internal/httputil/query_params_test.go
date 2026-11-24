// Copyright 2026 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package httputil

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRejectRepeatedQueryParams(t *testing.T) {
	tests := []struct {
		query string
		want  string
	}{
		{query: ""},
		{query: "dir=b&from=x&limit=5"},
		{query: "via=a.com&via=b.com"},
		{query: "org.matrix.msc4235.via=a.com&org.matrix.msc4235.via=b.com"},
		{query: "server_name=a.com&server_name=b.com"},
		{query: "ver=10&ver=11"},
		{query: "v=$a&v=$b"},
		{query: "dir=b&dir=f", want: "dir"},
		{query: "dir=b&dir=b", want: "dir"},
		{query: "limit=1&limit=2&from=a&from=b", want: "from"},
		{query: "access_token=a&access_token=b", want: "access_token"},
	}
	for _, tc := range tests {
		t.Run(tc.query, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/?"+tc.query, nil)
			res := RejectRepeatedQueryParams(req)
			if tc.want == "" {
				assert.Nil(t, res)
				return
			}
			require.NotNil(t, res)
			assert.Equal(t, http.StatusBadRequest, res.Code)
			matrixErr, ok := res.JSON.(spec.MatrixError)
			require.True(t, ok, "JSON is %T", res.JSON)
			assert.Equal(t, spec.ErrorInvalidParam, matrixErr.ErrCode)
			assert.Contains(t, matrixErr.Err, tc.want)
		})
	}
}

func TestMakeExternalAPIRepeatedQueryParams(t *testing.T) {
	handler := func(*http.Request) util.JSONResponse {
		return util.JSONResponse{Code: http.StatusOK, JSON: struct{}{}}
	}
	tests := []struct {
		name     string
		h        http.Handler
		query    string
		wantCode int
	}{
		{name: "single values pass", h: MakeExternalAPI("test", handler), query: "dir=b", wantCode: http.StatusOK},
		{name: "repeated value rejected", h: MakeExternalAPI("test", handler), query: "dir=b&dir=f", wantCode: http.StatusBadRequest},
		{name: "opt-out passes", h: MakeExternalAPI("test", handler, WithAllowRepeatedQueryParams()), query: "dir=b&dir=f", wantCode: http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/?"+tc.query, nil))
			assert.Equal(t, tc.wantCode, rec.Code)
			if tc.wantCode == http.StatusBadRequest {
				var body spec.MatrixError
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
				assert.Equal(t, spec.ErrorInvalidParam, body.ErrCode)
			}
		})
	}
}
