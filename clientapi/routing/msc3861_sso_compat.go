// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only.

package routing

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/clientapi/auth"
	"codefloe.com/pat-s/zendrite/clientapi/auth/authtypes"
	clienthttputil "codefloe.com/pat-s/zendrite/clientapi/httputil"
	"codefloe.com/pat-s/zendrite/internal/httputil"
	"codefloe.com/pat-s/zendrite/setup/config"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// ssoCompatSession holds the ephemeral state for an in-flight legacy SSO -> OIDC
// login. It is keyed by the OAuth2 state parameter.
type ssoCompatSession struct {
	redirectURL  string
	codeVerifier string
	deviceID     string
	expiresAt    time.Time
}

// ssoCompatOIDCTokens holds the provider-issued credentials for a completed
// SSO login, keyed by the Matrix login token handed to the client. They are
// returned verbatim when the client redeems the login token via
// POST /login m.login.token: under MSC3861 a homeserver-local access token
// would never validate, since token validation is delegated to the OIDC
// provider.
type ssoCompatOIDCTokens struct {
	accessToken  string
	refreshToken string
	expiresInMs  int
	deviceID     string
	userID       string
	expiresAt    time.Time
}

// ssoCompatHandler bridges Element's legacy m.login.sso flow to the OIDC
// provider configured for MSC3861.
type ssoCompatHandler struct {
	cfg        *config.MSC3861Config
	matrixCfg  *config.Global
	userAPI    userapi.ClientUserAPI
	httpClient *http.Client
	rateLimits *httputil.RateLimits

	mu           sync.Mutex
	sessions     map[string]*ssoCompatSession
	oidcByLogTok map[string]*ssoCompatOIDCTokens
}

const (
	ssoSessionTTL      = 10 * time.Minute
	ssoCleanupInterval = 5 * time.Minute
	// Device IDs allocated for legacy SSO logins are random strings of this
	// length; 16 alphanumeric characters is comfortably unique per user, per
	// the device ID allocation guidance in the OAuth 2.0 API spec.
	ssoDeviceIDLength = 16
	// Converts the provider's expires_in seconds to the expires_in_ms
	// milliseconds used by the Matrix client API.
	millisPerSecond = 1000
)

func newSSOCompatHandler(
	cfg *config.MSC3861Config,
	matrixCfg *config.Global,
	userAPI userapi.ClientUserAPI,
	httpClient *http.Client,
	rateLimits *httputil.RateLimits,
) *ssoCompatHandler {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	h := &ssoCompatHandler{
		cfg:          cfg,
		matrixCfg:    matrixCfg,
		userAPI:      userAPI,
		httpClient:   httpClient,
		rateLimits:   rateLimits,
		sessions:     make(map[string]*ssoCompatSession),
		oidcByLogTok: make(map[string]*ssoCompatOIDCTokens),
	}
	go h.cleanupLoop()
	return h
}

func (h *ssoCompatHandler) cleanupLoop() {
	ticker := time.NewTicker(ssoCleanupInterval)
	defer ticker.Stop()
	for range ticker.C {
		h.cleanupExpiredSessions()
	}
}

func (h *ssoCompatHandler) cleanupExpiredSessions() {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	for k, v := range h.sessions {
		if now.After(v.expiresAt) {
			delete(h.sessions, k)
		}
	}
	for k, v := range h.oidcByLogTok {
		if now.After(v.expiresAt) {
			delete(h.oidcByLogTok, k)
		}
	}
}

func (h *ssoCompatHandler) registerRedirect(redirectURL string) (state, codeVerifier, deviceID string, err error) {
	state, err = auth.GenerateAccessToken()
	if err != nil {
		return "", "", "", fmt.Errorf("failed to generate SSO state: %w", err)
	}
	codeVerifier, err = generatePKCEVerifier()
	if err != nil {
		return "", "", "", fmt.Errorf("failed to generate PKCE verifier: %w", err)
	}
	deviceID = util.RandomString(ssoDeviceIDLength)

	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessions[state] = &ssoCompatSession{
		redirectURL:  redirectURL,
		codeVerifier: codeVerifier,
		// The device ID is allocated up front so it can be requested via the
		// urn:matrix:client:device: scope, as native OAuth 2.0 clients do. Each
		// SSO login therefore gets its own device instead of sharing the
		// fallback "OIDC" device.
		deviceID:  deviceID,
		expiresAt: time.Now().Add(ssoSessionTTL),
	}
	return state, codeVerifier, deviceID, nil
}

func (h *ssoCompatHandler) getSession(state string) *ssoCompatSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.sessions[state]; ok && time.Now().Before(s.expiresAt) {
		return s
	}
	return nil
}

func (h *ssoCompatHandler) deleteSession(state string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.sessions, state)
}

// stashOIDCTokens records the provider-issued credentials for a completed SSO
// login, keyed by the Matrix login token the client will redeem.
func (h *ssoCompatHandler) stashOIDCTokens(loginToken string, tokens *oidcTokenResponse, deviceID, userID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.oidcByLogTok[loginToken] = &ssoCompatOIDCTokens{
		accessToken:  tokens.AccessToken,
		refreshToken: tokens.RefreshToken,
		expiresInMs:  tokens.ExpiresIn * millisPerSecond,
		deviceID:     deviceID,
		userID:       userID,
		expiresAt:    time.Now().Add(ssoSessionTTL),
	}
}

// takeOIDCTokens returns and removes the stashed credentials for a login
// token, or nil if the token is unknown or expired.
func (h *ssoCompatHandler) takeOIDCTokens(loginToken string) *ssoCompatOIDCTokens {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.oidcByLogTok[loginToken]
	if !ok || time.Now().After(t.expiresAt) {
		return nil
	}
	delete(h.oidcByLogTok, loginToken)
	return t
}

// generatePKCEVerifier creates a 32-byte URL-safe base64 string (43 chars),
// suitable for PKCE code challenge generation.
func generatePKCEVerifier() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func pkceChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// requestScheme returns the URL scheme for an incoming request, honoring
// X-Forwarded-Proto when present.
func requestScheme(req *http.Request) string {
	if fwd := req.Header.Get("X-Forwarded-Proto"); fwd != "" {
		return fwd
	}
	if req.TLS != nil {
		return "https"
	}
	return "http"
}

// callbackURL returns the registered OIDC callback URL for this homeserver.
func (h *ssoCompatHandler) callbackURL(req *http.Request) string {
	if h.cfg.SSOCallbackURL != "" {
		return h.cfg.SSOCallbackURL
	}
	return requestScheme(req) + "://" + req.Host + "/_matrix/client/v3/login/sso/callback"
}

// serveRedirect handles GET /_matrix/client/v3/login/sso/redirect.
func (h *ssoCompatHandler) serveRedirect(req *http.Request) util.JSONResponse {
	if r := h.rateLimits.Limit(req, nil); r != nil {
		return *r
	}

	redirectURL := req.URL.Query().Get("redirectUrl")
	if redirectURL == "" {
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.BadJSON("Missing redirectUrl query parameter."),
		}
	}

	state, codeVerifier, deviceID, err := h.registerRedirect(redirectURL)
	if err != nil {
		logrus.WithError(err).Error("MSC3861 SSO compat: failed to create session")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}

	endpoints, err := h.discoverOIDCEndpoints(req.Context())
	if err != nil {
		logrus.WithError(err).Error("MSC3861 SSO compat: failed to discover OIDC endpoints")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}

	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {h.cfg.ClientID},
		"redirect_uri":          {h.callbackURL(req)},
		"state":                 {state},
		"scope":                 {"openid offline_access urn:matrix:client:api:* urn:matrix:client:device:" + deviceID},
		"code_challenge":        {pkceChallenge(codeVerifier)},
		"code_challenge_method": {"S256"},
	}
	// OAuth 2.0 aware clients signal the user's intent with the action
	// parameter; registration maps to prompt=create at the provider.
	if req.URL.Query().Get("action") == "register" {
		q.Set("prompt", "create")
	}
	authURL := endpoints.authorizationEndpoint + "?" + q.Encode()

	return util.JSONResponse{
		Code:    http.StatusFound,
		Headers: map[string]string{"Location": authURL},
	}
}

// serveCallback handles GET /_matrix/client/v3/login/sso/callback.
func (h *ssoCompatHandler) serveCallback(req *http.Request) util.JSONResponse {
	state := req.URL.Query().Get("state")
	session := h.getSession(state)
	if session == nil {
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.BadJSON("Invalid or expired SSO session state."),
		}
	}
	defer h.deleteSession(state)

	if errCode := req.URL.Query().Get("error"); errCode != "" {
		errDesc := req.URL.Query().Get("error_description")
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.Unknown("OIDC provider error: " + errCode + ": " + errDesc),
		}
	}

	code := req.URL.Query().Get("code")
	if code == "" {
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.BadJSON("Missing authorization code."),
		}
	}

	tokens, err := h.exchangeCode(req.Context(), code, h.callbackURL(req), session.codeVerifier)
	if err != nil {
		logrus.WithError(err).Error("MSC3861 SSO compat: token exchange failed")
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.Unknown("Failed to exchange OIDC authorization code."),
		}
	}

	device, errRes := h.provisionUser(req.Context(), tokens.AccessToken)
	if errRes != nil {
		return *errRes
	}

	var tokenRes userapi.PerformLoginTokenCreationResponse
	if err := h.userAPI.PerformLoginTokenCreation(req.Context(), &userapi.PerformLoginTokenCreationRequest{
		Data: userapi.LoginTokenData{UserID: device.UserID},
	}, &tokenRes); err != nil {
		logrus.WithError(err).Error("MSC3861 SSO compat: failed to create login token")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}

	// Stash the provider-issued credentials against the login token so that
	// redeeming it via m.login.token returns the OIDC access token rather
	// than a homeserver-local one, which would never introspect.
	h.stashOIDCTokens(tokenRes.Metadata.Token, tokens, device.ID, device.UserID)

	clientURL, err := url.Parse(session.redirectURL)
	if err != nil {
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.Unknown("Invalid redirectUrl."),
		}
	}
	q := clientURL.Query()
	q.Set("loginToken", tokenRes.Metadata.Token)
	clientURL.RawQuery = q.Encode()

	return util.JSONResponse{
		Code:    http.StatusFound,
		Headers: map[string]string{"Location": clientURL.String()},
	}
}

type oidcProviderEndpoints struct {
	authorizationEndpoint string
	tokenEndpoint         string
}

func (h *ssoCompatHandler) discoverOIDCEndpoints(ctx context.Context) (*oidcProviderEndpoints, error) {
	cfg := h.cfg
	if cfg.AuthorizationEndpoint != "" && cfg.TokenEndpoint != "" {
		return &oidcProviderEndpoints{
			authorizationEndpoint: cfg.AuthorizationEndpoint,
			tokenEndpoint:         cfg.TokenEndpoint,
		}, nil
	}

	discoveryURL := strings.TrimSuffix(cfg.Issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("OIDC discovery returned status %d: %s", resp.StatusCode, string(body))
	}

	var doc struct {
		AuthorizationEndpoint string `json:"authorization_endpoint"`
		TokenEndpoint         string `json:"token_endpoint"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, err
	}

	authz := doc.AuthorizationEndpoint
	if authz == "" {
		authz = cfg.AuthorizationEndpointOrDefault()
	}
	token := doc.TokenEndpoint
	if token == "" {
		token = cfg.TokenEndpointOrDefault()
	}
	return &oidcProviderEndpoints{
		authorizationEndpoint: authz,
		tokenEndpoint:         token,
	}, nil
}

type oidcTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
}

// callTokenEndpoint POSTs a form to the provider's token endpoint,
// authenticating with the configured client credentials.
func (h *ssoCompatHandler) callTokenEndpoint(ctx context.Context, endpoint string, form url.Values) (*http.Response, error) {
	authMethod := h.cfg.ClientAuthMethod
	if authMethod == "" {
		authMethod = "client_secret_basic"
	}

	var bodyReader io.Reader
	switch authMethod {
	case "client_secret_basic":
		// body is the base form; basic auth added below.
		bodyReader = strings.NewReader(form.Encode())
	case "client_secret_post":
		form.Set("client_secret", h.cfg.ClientSecret)
		bodyReader = strings.NewReader(form.Encode())
	default:
		return nil, fmt.Errorf("unsupported client_auth_method %q", authMethod)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if authMethod == "client_secret_basic" {
		req.SetBasicAuth(url.QueryEscape(h.cfg.ClientID), url.QueryEscape(h.cfg.ClientSecret))
	}

	return h.httpClient.Do(req)
}

func (h *ssoCompatHandler) exchangeCode(ctx context.Context, code, callbackURL, codeVerifier string) (*oidcTokenResponse, error) {
	endpoints, err := h.discoverOIDCEndpoints(ctx)
	if err != nil {
		return nil, err
	}

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {callbackURL},
		"client_id":     {h.cfg.ClientID},
		"code_verifier": {codeVerifier},
	}

	resp, err := h.callTokenEndpoint(ctx, endpoints.tokenEndpoint, form)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("token endpoint returned status %d: %s", resp.StatusCode, string(body))
	}

	var tokens oidcTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokens); err != nil {
		return nil, err
	}
	if tokens.AccessToken == "" {
		return nil, fmt.Errorf("token endpoint response did not contain access_token")
	}
	return &tokens, nil
}

// refreshRequest is the request body for POST /_matrix/client/v3/refresh.
type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// refreshResponse is the response body for POST /_matrix/client/v3/refresh.
type refreshResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresInMs  int    `json:"expires_in_ms,omitempty"`
}

// serveRefresh handles POST /_matrix/client/v3/refresh when MSC3861 is
// enabled, bridging the legacy refresh endpoint to the provider's refresh
// token grant. Without it, every legacy SSO session would die when the
// provider-issued access token expires.
func (h *ssoCompatHandler) serveRefresh(req *http.Request) util.JSONResponse {
	if r := h.rateLimits.Limit(req, nil); r != nil {
		return *r
	}

	var refreshReq refreshRequest
	if resErr := clienthttputil.UnmarshalJSONRequest(req, &refreshReq); resErr != nil {
		return *resErr
	}
	if refreshReq.RefreshToken == "" {
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.MissingParam("Missing refresh_token"),
		}
	}

	endpoints, err := h.discoverOIDCEndpoints(req.Context())
	if err != nil {
		logrus.WithError(err).Error("MSC3861: failed to discover OIDC endpoints for token refresh")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshReq.RefreshToken},
		"client_id":     {h.cfg.ClientID},
	}

	resp, err := h.callTokenEndpoint(req.Context(), endpoints.tokenEndpoint, form)
	if err != nil {
		logrus.WithError(err).Error("MSC3861: refresh request to provider failed")
		return util.JSONResponse{
			Code: http.StatusBadGateway,
			JSON: spec.InternalServerError{Err: "failed to reach the OIDC provider"},
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
		// The provider rejected the refresh token (e.g. invalid_grant); the
		// session is gone.
		return util.JSONResponse{
			Code: http.StatusUnauthorized,
			JSON: spec.UnknownToken("Refresh token was rejected by the OIDC provider"),
		}
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		logrus.WithFields(logrus.Fields{
			"status":   resp.StatusCode,
			"response": string(body),
		}).Error("MSC3861: provider token refresh returned non-200 status")
		return util.JSONResponse{
			Code: http.StatusBadGateway,
			JSON: spec.InternalServerError{Err: "OIDC provider failed to refresh the token"},
		}
	}

	var tokens oidcTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokens); err != nil || tokens.AccessToken == "" {
		return util.JSONResponse{
			Code: http.StatusBadGateway,
			JSON: spec.InternalServerError{Err: "invalid token response from the OIDC provider"},
		}
	}

	// Providers may rotate refresh tokens; if they don't, hand back the one
	// the client sent.
	newRefreshToken := tokens.RefreshToken
	if newRefreshToken == "" {
		newRefreshToken = refreshReq.RefreshToken
	}

	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: refreshResponse{
			AccessToken:  tokens.AccessToken,
			RefreshToken: newRefreshToken,
			ExpiresInMs:  tokens.ExpiresIn * millisPerSecond,
		},
	}
}

// provisionUser validates the OIDC access token and returns the Matrix device
// it represents. It uses QueryAccessToken which performs RFC 7662 token
// introspection (with a userinfo fallback) and auto-provisions the user account
// and device if necessary.
func (h *ssoCompatHandler) provisionUser(ctx context.Context, accessToken string) (*userapi.Device, *util.JSONResponse) {
	var res userapi.QueryAccessTokenResponse
	if err := h.userAPI.QueryAccessToken(ctx, &userapi.QueryAccessTokenRequest{
		AccessToken: accessToken,
	}, &res); err != nil {
		logrus.WithError(err).Error("MSC3861 SSO compat: QueryAccessToken failed")
		return nil, &util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{Err: "failed to look up user"},
		}
	}
	if res.Err != "" {
		logrus.WithField("query_access_token_err", res.Err).Error("MSC3861 SSO compat: QueryAccessToken returned error")
		return nil, &util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{Err: "failed to look up user"},
		}
	}
	if res.Device == nil {
		logrus.Error("MSC3861 SSO compat: OIDC access token is not active")
		return nil, &util.JSONResponse{
			Code: http.StatusUnauthorized,
			JSON: spec.UnknownToken("OIDC access token is not active"),
		}
	}
	return res.Device, nil
}

// msc3861LoginFlows returns the login flows response when MSC3861 is active.
// When the legacy SSO compat layer is enabled we advertise m.login.sso and
// m.login.token so that clients which do not yet implement native OIDC can
// still log in via the OIDC provider. The SSO flow is marked as
// oauth_aware_preferred so OAuth 2.0 aware clients only offer that flow.
func msc3861LoginFlows(_ *config.MSCs) util.JSONResponse {
	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: flows{
			Flows: []flow{
				{Type: authtypes.LoginTypeSSO, OAuthAwarePreferred: true},
				{Type: authtypes.LoginTypeToken},
			},
		},
	}
}

// serveLogin handles /_matrix/client/v3/login when MSC3861 is enabled. GET
// returns the OIDC-aware flow list; POST only accepts m.login.token and
// redeems it against the provider-issued credentials stashed by the SSO
// callback. Login tokens unknown to the SSO compat layer are rejected:
// redeeming them through the legacy login path would mint a homeserver-local
// access token that can never validate against the OIDC provider.
func (h *ssoCompatHandler) serveLogin(cfg *config.ClientAPI) http.Handler {
	return httputil.MakeExternalAPI("login", func(req *http.Request) util.JSONResponse {
		if req.Method == http.MethodGet || req.Method == http.MethodOptions {
			return msc3861LoginFlows(cfg.MSCs)
		}

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
		_ = req.Body.Close()

		// Only m.login.token is allowed under MSC3861; everything else is delegated.
		if gjson.GetBytes(body, "type").String() != authtypes.LoginTypeToken {
			return util.JSONResponse{
				Code: http.StatusForbidden,
				JSON: spec.Forbidden("Login is delegated to the OIDC provider via MSC3861."),
			}
		}

		if r := h.rateLimits.Limit(req, nil); r != nil {
			return *r
		}

		loginToken := gjson.GetBytes(body, "token").String()
		if loginToken == "" {
			return util.JSONResponse{
				Code: http.StatusBadRequest,
				JSON: spec.MissingParam("Missing token"),
			}
		}

		oidcTokens := h.takeOIDCTokens(loginToken)
		if oidcTokens == nil {
			return util.JSONResponse{
				Code: http.StatusForbidden,
				JSON: spec.Forbidden("Invalid or expired login token"),
			}
		}

		// Validate and consume the login token so it cannot be replayed.
		var queryRes userapi.QueryLoginTokenResponse
		if err := h.userAPI.QueryLoginToken(req.Context(), &userapi.QueryLoginTokenRequest{
			Token: loginToken,
		}, &queryRes); err != nil || queryRes.Data == nil {
			return util.JSONResponse{
				Code: http.StatusForbidden,
				JSON: spec.Forbidden("Invalid or expired login token"),
			}
		}
		var delRes userapi.PerformLoginTokenDeletionResponse
		if err := h.userAPI.PerformLoginTokenDeletion(req.Context(), &userapi.PerformLoginTokenDeletionRequest{
			Token: loginToken,
		}, &delRes); err != nil {
			logrus.WithError(err).Error("MSC3861 SSO compat: failed to delete login token")
		}

		return util.JSONResponse{
			Code: http.StatusOK,
			JSON: loginResponse{
				UserID:       oidcTokens.userID,
				AccessToken:  oidcTokens.accessToken,
				DeviceID:     oidcTokens.deviceID,
				RefreshToken: oidcTokens.refreshToken,
				ExpiresInMs:  oidcTokens.expiresInMs,
			},
		}
	})
}
