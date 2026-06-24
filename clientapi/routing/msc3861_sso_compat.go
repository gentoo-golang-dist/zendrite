// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only.

package routing

import (
	"bytes"
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
	"codefloe.com/pat-s/zendrite/internal/httputil"
	"codefloe.com/pat-s/zendrite/setup/config"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// ssoCompatSession holds the ephemeral state for an in-flight legacy SSO -> OIDC
// login. It is keyed by the OAuth2 state parameter.
type ssoCompatSession struct {
	redirectURL  string
	codeVerifier string
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

	mu       sync.Mutex
	sessions map[string]*ssoCompatSession
}

const (
	ssoSessionTTL      = 10 * time.Minute
	ssoCleanupInterval = 5 * time.Minute
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
		cfg:        cfg,
		matrixCfg:  matrixCfg,
		userAPI:    userAPI,
		httpClient: httpClient,
		rateLimits: rateLimits,
		sessions:   make(map[string]*ssoCompatSession),
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
}

func (h *ssoCompatHandler) registerRedirect(redirectURL string) (state, codeVerifier string, err error) {
	state, err = auth.GenerateAccessToken()
	if err != nil {
		return "", "", fmt.Errorf("failed to generate SSO state: %w", err)
	}
	codeVerifier, err = generatePKCEVerifier()
	if err != nil {
		return "", "", fmt.Errorf("failed to generate PKCE verifier: %w", err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessions[state] = &ssoCompatSession{
		redirectURL:  redirectURL,
		codeVerifier: codeVerifier,
		expiresAt:    time.Now().Add(ssoSessionTTL),
	}
	return state, codeVerifier, nil
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

	state, codeVerifier, err := h.registerRedirect(redirectURL)
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
		"scope":                 {"openid urn:matrix:client:api:*"},
		"code_challenge":        {pkceChallenge(codeVerifier)},
		"code_challenge_method": {"S256"},
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

	userID, err := h.provisionUser(req.Context(), tokens.AccessToken)
	if err != nil {
		logrus.WithError(err).Error("MSC3861 SSO compat: user provisioning failed")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}

	var tokenRes userapi.PerformLoginTokenCreationResponse
	if err := h.userAPI.PerformLoginTokenCreation(req.Context(), &userapi.PerformLoginTokenCreationRequest{
		Data: userapi.LoginTokenData{UserID: userID},
	}, &tokenRes); err != nil {
		logrus.WithError(err).Error("MSC3861 SSO compat: failed to create login token")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}

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
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoints.tokenEndpoint, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if authMethod == "client_secret_basic" {
		req.SetBasicAuth(url.QueryEscape(h.cfg.ClientID), url.QueryEscape(h.cfg.ClientSecret))
	}

	resp, err := h.httpClient.Do(req)
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

func (h *ssoCompatHandler) provisionUser(ctx context.Context, accessToken string) (string, error) {
	var res userapi.QueryAccessTokenResponse
	if err := h.userAPI.QueryAccessToken(ctx, &userapi.QueryAccessTokenRequest{
		AccessToken: accessToken,
	}, &res); err != nil {
		return "", err
	}
	if res.Device == nil {
		return "", fmt.Errorf("OIDC access token is not active")
	}
	return res.Device.UserID, nil
}

// msc3861LoginFlows returns the login flows response when MSC3861 is active.
// When the legacy SSO compat layer is enabled we advertise m.login.sso and
// m.login.token so that clients which do not yet implement native OIDC can
// still log in via the OIDC provider.
func msc3861LoginFlows(_ *config.MSCs) util.JSONResponse {
	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: flows{
			Flows: []flow{
				{Type: authtypes.LoginTypeSSO},
				{Type: authtypes.LoginTypeToken},
			},
		},
	}
}

// msc3861LoginHandler returns a handler for /_matrix/client/v3/login when
// MSC3861 is enabled. GET returns the OIDC-aware flow list; POST only accepts
// m.login.token (used to redeem a login token issued by the SSO callback).
func msc3861LoginHandler(
	cfg *config.ClientAPI,
	userAPI userapi.ClientUserAPI,
	rateLimits *httputil.RateLimits,
) http.Handler {
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
		req.Body = io.NopCloser(bytes.NewReader(body))

		// Only m.login.token is allowed under MSC3861; everything else is delegated.
		if gjson.GetBytes(body, "type").String() != authtypes.LoginTypeToken {
			return util.JSONResponse{
				Code: http.StatusForbidden,
				JSON: spec.Forbidden("Login is delegated to the OIDC provider via MSC3861."),
			}
		}

		if r := rateLimits.Limit(req, nil); r != nil {
			return *r
		}
		return Login(req, userAPI, cfg)
	})
}
