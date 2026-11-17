// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/userapi/api"
)

// unknownTokenAPI knows no access token.
type unknownTokenAPI struct{}

func (unknownTokenAPI) QueryAccessToken(context.Context, *api.QueryAccessTokenRequest, *api.QueryAccessTokenResponse) error {
	return nil
}

// TestQueryStringAuth checks the access_token query parameter with and
// without client_api.disable_query_string_auth (MSC4127). It changes the
// package setting, so it does not run in parallel.
func TestQueryStringAuth(t *testing.T) {
	defer SetQueryStringAuth(true)

	request := func(header, query string) *http.Request {
		target := "/_matrix/client/v3/account/whoami"
		if query != "" {
			target += "?access_token=" + query
		}
		req := httptest.NewRequest(http.MethodGet, target, nil)
		if header != "" {
			req.Header.Set("Authorization", "Bearer "+header)
		}
		return req
	}

	counted := func() float64 {
		var metric dto.Metric
		require.NoError(t, queryStringAuthRequests.Write(&metric))
		return metric.GetCounter().GetValue()
	}

	SetQueryStringAuth(true)
	assert.True(t, QueryStringAuthEnabled())
	token, err := ExtractAccessToken(request("", "query"))
	require.NoError(t, err)
	assert.Equal(t, "query", token)
	_, err = ExtractAccessToken(request("header", "query"))
	assert.Error(t, err, "mixing both is refused")

	// Requests that rely on the parameter alone are counted, once each.
	before := counted()
	_, _ = VerifyUserFromRequest(request("", "query"), unknownTokenAPI{})
	_, _ = VerifyUserFromRequest(request("header", "query"), unknownTokenAPI{})
	_, _ = VerifyUserFromRequest(request("header", ""), unknownTokenAPI{})
	assert.InDelta(t, before+1, counted(), 0)
	assert.True(t, RelyOnQueryStringAuth(request("", "query")))
	assert.False(t, RelyOnQueryStringAuth(request("header", "query")))

	SetQueryStringAuth(false)
	assert.False(t, QueryStringAuthEnabled())
	_, err = ExtractAccessToken(request("", "query"))
	assert.EqualError(t, err, "the access_token query parameter is not supported, use the Authorization header")
	token, err = ExtractAccessToken(request("header", "query"))
	require.NoError(t, err, "the parameter is ignored next to the header")
	assert.Equal(t, "header", token)
	token, err = ExtractAccessToken(request("header", ""))
	require.NoError(t, err)
	assert.Equal(t, "header", token)
}
