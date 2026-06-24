// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only.

package routing

import (
	"bytes"
	"io"
	"net/http"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/clientapi/auth"
	"codefloe.com/pat-s/zendrite/clientapi/auth/authtypes"
	"codefloe.com/pat-s/zendrite/internal/httputil"
	"codefloe.com/pat-s/zendrite/setup/config"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// msc3861ForbiddenHandler returns an HTTP handler that rejects requests with
// M_FORBIDDEN when MSC3861 OIDC delegated authentication is enabled.
func msc3861ForbiddenHandler(metricsName string) http.Handler {
	return httputil.MakeExternalAPI(metricsName, func(req *http.Request) util.JSONResponse {
		return util.JSONResponse{
			Code: http.StatusForbidden,
			JSON: spec.Forbidden("This endpoint is not available when authentication is delegated to an OIDC provider via MSC3861."),
		}
	})
}

// msc3861RegisterHandler returns a handler for POST /_matrix/client/v3/register
// when MSC3861 is enabled. Application-service registrations
// (m.login.application_service with an access token) are still allowed; all
// other registration attempts are rejected.
func msc3861RegisterHandler(
	cfg *config.ClientAPI,
	userAPI userapi.ClientUserAPI,
	rateLimits *httputil.RateLimits,
) http.Handler {
	return httputil.MakeExternalAPI("register", func(req *http.Request) util.JSONResponse {
		if req.Body == nil {
			req.Body = http.NoBody
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return util.JSONResponse{
				Code: http.StatusBadRequest,
				JSON: spec.NotJSON("Unable to read request body"),
			}
		}
		req.Body.Close()
		// Restore the body so the real registration handler can read it.
		req.Body = io.NopCloser(bytes.NewReader(body))

		// Allow application-service registrations through.
		_, accessTokenErr := auth.ExtractAccessToken(req)
		if gjson.GetBytes(body, "type").String() == authtypes.LoginTypeApplicationService && accessTokenErr == nil {
			return Register(req, userAPI, cfg)
		}

		// Otherwise apply the usual rate limit and reject.
		if r := rateLimits.Limit(req, nil); r != nil {
			return *r
		}
		return util.JSONResponse{
			Code: http.StatusForbidden,
			JSON: spec.Forbidden("Registration is delegated to the OIDC provider via MSC3861."),
		}
	})
}

// AuthMetadataResponse is the RFC8414-style authorization server metadata
// returned by GET /_matrix/client/v1/auth_metadata (MSC2965).
type AuthMetadataResponse struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint"`
	RevocationEndpoint                string   `json:"revocation_endpoint"`
	JWKSURI                           string   `json:"jwks_uri"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	ResponseModesSupported            []string `json:"response_modes_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	ScopesSupported                   []string `json:"scopes_supported"`
}

// msc3861AuthMetadata returns the authorization server metadata when MSC3861 is active.
func msc3861AuthMetadata(mscCfg *config.MSCs) util.JSONResponse {
	cfg := &mscCfg.MSC3861
	return util.JSONResponse{
		Code: http.StatusOK,
		Headers: map[string]string{
			"Cache-Control": "public, max-age=3600",
		},
		JSON: AuthMetadataResponse{
			Issuer:                            cfg.Issuer,
			AuthorizationEndpoint:             cfg.AuthorizationEndpointOrDefault(),
			TokenEndpoint:                     cfg.TokenEndpointOrDefault(),
			RegistrationEndpoint:              cfg.RegistrationEndpointOrDefault(),
			RevocationEndpoint:                cfg.RevocationEndpointOrDefault(),
			JWKSURI:                           cfg.JWKSURIOrDefault(),
			ResponseTypesSupported:            []string{"code"},
			GrantTypesSupported:               []string{"authorization_code", "refresh_token"},
			ResponseModesSupported:            []string{"query", "fragment"},
			CodeChallengeMethodsSupported:     []string{"S256"},
			TokenEndpointAuthMethodsSupported: []string{"client_secret_basic", "client_secret_post"},
			ScopesSupported: []string{
				"openid",
				"urn:matrix:client:api:*",
			},
		},
	}
}
