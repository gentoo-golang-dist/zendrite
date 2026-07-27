// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only.

package internal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jellydator/ttlcache/v3"
	"github.com/sirupsen/logrus"

	zinternal "codefloe.com/pat-s/zendrite/internal"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/userapi/api"
)

// IntrospectionResponse represents the response from an RFC 7662 token introspection endpoint.
type IntrospectionResponse struct {
	Active   bool   `json:"active"`
	Sub      string `json:"sub"`
	Scope    string `json:"scope"`
	Exp      int64  `json:"exp"`
	Username string `json:"username"`
	// Email is a non-standard but RFC 7662-permitted member some providers
	// inject so the homeserver can derive a friendlier localpart than the
	// subject claim.
	Email string `json:"email"`
}

// oidcDiscoveryResponse represents the relevant fields from an OIDC discovery document.
type oidcDiscoveryResponse struct {
	IntrospectionEndpoint string `json:"introspection_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

// cachedIntrospection holds a cached introspection result.
type cachedIntrospection struct {
	response *IntrospectionResponse
	err      error
}

var (
	// IntrospectionCache caches token introspection results for 30 seconds.
	introspectionCache     *ttlcache.Cache[string, *cachedIntrospection]
	introspectionCacheOnce sync.Once

	// DiscoveredEndpoints caches the discovered introspection endpoint per issuer.
	discoveredEndpoints   = make(map[string]*oidcDiscoveryCache)
	discoveredEndpointsMu sync.Mutex
)

type oidcDiscoveryCache struct {
	endpoint string
	cachedAt time.Time
}

const (
	introspectionCacheTTL     = 30 * time.Second
	introspectionCacheMaxSize = 10_000
	discoveryEndpointCacheTTL = 1 * time.Hour
	stableDeviceScopePrefix   = "urn:matrix:client:device:"
	deviceScopePrefix         = "urn:matrix:device:"
	legacyDeviceScopePrefix   = "urn:matrix:org.matrix.msc2967.client:device:"
)

func getIntrospectionCache() *ttlcache.Cache[string, *cachedIntrospection] {
	introspectionCacheOnce.Do(func() {
		introspectionCache = ttlcache.New[string, *cachedIntrospection](
			ttlcache.WithTTL[string, *cachedIntrospection](introspectionCacheTTL),
			ttlcache.WithCapacity[string, *cachedIntrospection](introspectionCacheMaxSize),
		)
		go introspectionCache.Start()
	})
	return introspectionCache
}

// hashToken produces a SHA-256 hex hash of a token for use as a cache key.
func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// extractDeviceIDFromScope parses the OAuth scope string for a device ID
// encoded as urn:matrix:client:device:DEVICEID (the stable scope token from
// the OAuth 2.0 API), urn:matrix:device:DEVICEID, or the legacy unstable
// MSC2967 prefix.
func extractDeviceIDFromScope(scope string) string {
	for _, part := range strings.Fields(scope) {
		switch {
		case strings.HasPrefix(part, stableDeviceScopePrefix):
			return strings.TrimPrefix(part, stableDeviceScopePrefix)
		case strings.HasPrefix(part, deviceScopePrefix):
			return strings.TrimPrefix(part, deviceScopePrefix)
		case strings.HasPrefix(part, legacyDeviceScopePrefix):
			return strings.TrimPrefix(part, legacyDeviceScopePrefix)
		}
	}
	return ""
}

var localpartInvalidChars = regexp.MustCompile(`[^0-9a-z_\-+=./]+`)

// sanitizeLocalpart converts an arbitrary OIDC username claim into a valid
// Matrix localpart. Invalid characters are replaced with underscores, the
// result is lowercased, leading underscores are avoided, and it is trimmed
// to a sensible length. An empty input yields an empty result.
func sanitizeLocalpart(input string) string {
	if strings.TrimSpace(input) == "" {
		return ""
	}

	s := strings.ToLower(input)
	s = localpartInvalidChars.ReplaceAllString(s, "_")
	// Trim trailing underscores; leading underscores are handled below.
	s = strings.TrimRight(s, "_")

	if strings.HasPrefix(s, "_") {
		s = "u" + s
	}

	const maxLocalpartLen = 64
	if len(s) > maxLocalpartLen {
		s = s[:maxLocalpartLen]
	}

	return s
}

// stripEmailDomain removes the domain from an email address when it matches
// the server name, so that sienna@example.com provisions as @sienna:example.com
// rather than @sienna_example.com:example.com. Addresses on other domains are
// returned whole to keep localparts unique.
func stripEmailDomain(email, serverName string) string {
	local, domain, found := strings.Cut(email, "@")
	if found && strings.EqualFold(domain, serverName) {
		return local
	}
	return email
}

// deriveLocalpart picks the best Matrix localpart for an OIDC identity,
// preferring the email claim, then the username claim, then the subject
// claim for providers like Ory Hydra that don't expose a username.
func deriveLocalpart(introspection *IntrospectionResponse, serverName string) string {
	if localpart := sanitizeLocalpart(stripEmailDomain(introspection.Email, serverName)); localpart != "" {
		return localpart
	}
	if localpart := sanitizeLocalpart(introspection.Username); localpart != "" {
		return localpart
	}
	return sanitizeLocalpart(introspection.Sub)
}

// discoverIntrospectionEndpoint fetches the OIDC discovery document from the issuer
// and returns the introspection_endpoint. Results are cached for 1 hour per issuer.
func discoverIntrospectionEndpoint(ctx context.Context, issuer string, httpClient *http.Client) string {
	discoveredEndpointsMu.Lock()
	defer discoveredEndpointsMu.Unlock()

	issuer = strings.TrimSuffix(issuer, "/")
	if cached, ok := discoveredEndpoints[issuer]; ok && cached.endpoint != "" && time.Since(cached.cachedAt) < discoveryEndpointCacheTTL {
		return cached.endpoint
	}

	discoveryURL := issuer + "/.well-known/openid-configuration"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		logrus.WithError(err).Warn("MSC3861: failed to create OIDC discovery request")
		return ""
	}
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		logrus.WithError(err).Warn("MSC3861: OIDC discovery request failed")
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		logrus.WithField("status", resp.StatusCode).Warn("MSC3861: OIDC discovery returned non-200 status")
		return ""
	}

	var discovery oidcDiscoveryResponse
	if err := json.NewDecoder(resp.Body).Decode(&discovery); err != nil {
		logrus.WithError(err).Warn("MSC3861: failed to decode OIDC discovery response")
		return ""
	}

	if discovery.IntrospectionEndpoint != "" {
		discoveredEndpoints[issuer] = &oidcDiscoveryCache{
			endpoint: discovery.IntrospectionEndpoint,
			cachedAt: time.Now(),
		}
		logrus.WithField("endpoint", discovery.IntrospectionEndpoint).Info("MSC3861: discovered introspection endpoint")
		return discovery.IntrospectionEndpoint
	}

	return ""
}

// resolveIntrospectionEndpoint returns the introspection endpoint to use,
// trying config, then OIDC discovery, then the default fallback.
func resolveIntrospectionEndpoint(ctx context.Context, msc3861 *config.MSC3861Config, httpClient *http.Client) string {
	if msc3861.IntrospectionEndpoint != "" {
		return msc3861.IntrospectionEndpoint
	}

	if endpoint := discoverIntrospectionEndpoint(ctx, msc3861.Issuer, httpClient); endpoint != "" {
		return endpoint
	}

	return msc3861.IntrospectionEndpointOrDefault()
}

// discoverUserinfoEndpoint fetches the OIDC discovery document and returns the
// userinfo_endpoint, falling back to the configured or default issuer value.
func discoverUserinfoEndpoint(ctx context.Context, msc3861 *config.MSC3861Config, httpClient *http.Client) string {
	if msc3861.UserinfoEndpoint != "" {
		return msc3861.UserinfoEndpoint
	}

	if msc3861.Issuer == "" {
		return ""
	}

	discoveryURL := strings.TrimSuffix(msc3861.Issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		logrus.WithError(err).Warn("MSC3861: failed to create OIDC discovery request for userinfo")
		return msc3861.UserinfoEndpointOrDefault()
	}
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		logrus.WithError(err).Warn("MSC3861: OIDC discovery request for userinfo failed")
		return msc3861.UserinfoEndpointOrDefault()
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		logrus.WithField("status", resp.StatusCode).Warn("MSC3861: OIDC discovery for userinfo returned non-200 status")
		return msc3861.UserinfoEndpointOrDefault()
	}

	var discovery oidcDiscoveryResponse
	if err := json.NewDecoder(resp.Body).Decode(&discovery); err != nil {
		logrus.WithError(err).Warn("MSC3861: failed to decode OIDC discovery response for userinfo")
		return msc3861.UserinfoEndpointOrDefault()
	}

	if discovery.UserinfoEndpoint != "" {
		return discovery.UserinfoEndpoint
	}

	return msc3861.UserinfoEndpointOrDefault()
}

// errTokenExpired is returned when the OIDC provider rejects a token because
// it has expired, as opposed to being unknown or invalid for another reason.
// Callers use it to signal a soft logout so clients attempt a token refresh
// instead of destroying the session.
var errTokenExpired = fmt.Errorf("msc3861: access token expired")

// isExpiredBearerToken reports whether an RFC 6750 rejection indicates an
// expired (rather than unknown or malformed) token, checking both the
// WWW-Authenticate challenge and the response body for an invalid_token error
// that mentions expiry.
func isExpiredBearerToken(wwwAuthenticate string, body []byte) bool {
	haystack := strings.ToLower(wwwAuthenticate + " " + string(body))
	return strings.Contains(haystack, "invalid_token") && strings.Contains(haystack, "expired")
}

// userinfoToken validates an access token by calling the OIDC UserInfo endpoint.
// This is used as a fallback when RFC 7662 token introspection is unavailable.
func userinfoToken(ctx context.Context, msc3861 *config.MSC3861Config, token string, httpClient *http.Client) (*IntrospectionResponse, error) {
	endpoint := discoverUserinfoEndpoint(ctx, msc3861, httpClient)
	if endpoint == "" {
		return nil, fmt.Errorf("msc3861: no userinfo endpoint available")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("msc3861: failed to create userinfo request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("msc3861: userinfo request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		// A 401 with an invalid_token error describing expiry (RFC 6750) means
		// the session is intact but the token needs refreshing.
		if resp.StatusCode == http.StatusUnauthorized && isExpiredBearerToken(resp.Header.Get("WWW-Authenticate"), body) {
			return nil, errTokenExpired
		}
		return nil, fmt.Errorf("msc3861: userinfo returned status %d: %s", resp.StatusCode, string(body))
	}

	var userinfo struct {
		Sub               string `json:"sub"`
		PreferredUsername string `json:"preferred_username"`
		Name              string `json:"name"`
		Nickname          string `json:"nickname"`
		Email             string `json:"email"`
		Username          string `json:"username"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&userinfo); err != nil {
		return nil, fmt.Errorf("msc3861: failed to decode userinfo response: %w", err)
	}
	if userinfo.Sub == "" {
		return nil, fmt.Errorf("msc3861: userinfo response missing sub")
	}

	username := userinfo.PreferredUsername
	if username == "" {
		username = userinfo.Name
	}
	if username == "" {
		username = userinfo.Nickname
	}
	if username == "" {
		username = userinfo.Email
	}
	if username == "" {
		username = userinfo.Username
	}

	return &IntrospectionResponse{
		Active:   true,
		Sub:      userinfo.Sub,
		Username: username,
		Email:    userinfo.Email,
	}, nil
}

// introspectToken calls the OIDC introspection endpoint to validate an access token.
// If introspection is unavailable, it falls back to the OIDC UserInfo endpoint.
func introspectToken(ctx context.Context, msc3861 *config.MSC3861Config, token string, httpClient *http.Client) (*IntrospectionResponse, error) {
	// Check cache first.
	cache := getIntrospectionCache()
	tokenHash := hashToken(token)
	if item := cache.Get(tokenHash); item != nil {
		cached := item.Value()
		return cached.response, cached.err
	}

	endpoint := resolveIntrospectionEndpoint(ctx, msc3861, httpClient)

	form := url.Values{"token": {token}}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("msc3861: failed to create introspection request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	authMethod := msc3861.ClientAuthMethod
	if authMethod == "" {
		authMethod = "client_secret_basic"
	}

	switch authMethod {
	case "client_secret_basic":
		req.SetBasicAuth(url.QueryEscape(msc3861.ClientID), url.QueryEscape(msc3861.ClientSecret))
	case "client_secret_post":
		form.Set("client_id", msc3861.ClientID)
		form.Set("client_secret", msc3861.ClientSecret)
		req.Body = io.NopCloser(strings.NewReader(form.Encode()))
		req.ContentLength = int64(len(form.Encode()))
	default:
		return nil, fmt.Errorf("msc3861: unsupported client_auth_method %q", authMethod)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		logrus.WithError(err).Warn("MSC3861: introspection request failed, trying userinfo fallback")
		return userinfoToken(ctx, msc3861, token, httpClient)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		logrus.WithFields(logrus.Fields{
			"status":   resp.StatusCode,
			"response": string(body),
		}).Warn("MSC3861: introspection returned non-200 status, trying userinfo fallback")
		return userinfoToken(ctx, msc3861, token, httpClient)
	}

	var introspection IntrospectionResponse
	if err := json.NewDecoder(resp.Body).Decode(&introspection); err != nil {
		logrus.WithError(err).Warn("MSC3861: failed to decode introspection response, trying userinfo fallback")
		return userinfoToken(ctx, msc3861, token, httpClient)
	}

	// Cache the result (only cache active tokens and inactive responses, not errors).
	cache.Set(tokenHash, &cachedIntrospection{response: &introspection}, introspectionCacheTTL)

	return &introspection, nil
}

// queryAccessTokenMSC3861 handles token validation when MSC3861 OIDC delegation is enabled.
func (a *UserInternalAPI) queryAccessTokenMSC3861(ctx context.Context, req *api.QueryAccessTokenRequest, res *api.QueryAccessTokenResponse) error {
	msc3861 := &a.Config.MSCs.MSC3861

	// 1. Check static admin token.
	if msc3861.AdminToken != "" && req.AccessToken == msc3861.AdminToken {
		res.Device = &api.Device{
			ID:          "admin",
			UserID:      fmt.Sprintf("@admin:%s", a.Config.Matrix.ServerName),
			AccessToken: req.AccessToken,
			AccountType: api.AccountTypeAdmin,
		}
		return nil
	}

	// 2. Check appservice tokens (pass through unchanged).
	if req.AppServiceUserID != "" {
		appServiceDevice, err := a.queryAppServiceToken(ctx, req.AccessToken, req.AppServiceUserID)
		if err != nil || appServiceDevice != nil {
			if err != nil {
				res.Err = err.Error()
			}
			res.Device = appServiceDevice
			return nil
		}
	}

	// 3. Introspect the token with the OIDC provider.
	introspection, err := introspectToken(ctx, msc3861, req.AccessToken, a.HTTPClient)
	if err != nil {
		if errors.Is(err, errTokenExpired) {
			// The provider rejected the token as expired; tell the client it
			// can refresh instead of destroying the session.
			logrus.Warn("MSC3861: access token has expired (userinfo fallback)")
			res.SoftLogout = true
			return nil
		}
		logrus.WithError(err).Warn("MSC3861: token introspection failed")
		return nil
	}

	// 4. Inactive token -> return nil device (M_UNKNOWN_TOKEN).
	if !introspection.Active {
		return nil
	}

	// 5. Validate token expiry if present.
	if introspection.Exp > 0 && time.Now().Unix() > introspection.Exp {
		logrus.WithField("exp", introspection.Exp).Warn("MSC3861: access token has expired")
		// Tell the client it can refresh the token at the OIDC provider rather
		// than treating the session as destroyed.
		res.SoftLogout = true
		return nil
	}

	// 6. Resolve user identity via external ID mapping.
	sub := introspection.Sub
	if sub == "" {
		logrus.Warn("MSC3861: introspection response has empty sub")
		return nil
	}

	issuer := strings.TrimSuffix(msc3861.Issuer, "/")
	serverName := a.Config.Matrix.ServerName

	// Look up the external ID in the mapping table.
	localpart, _, err := a.DB.GetLocalpartByExternalID(ctx, issuer, sub)
	if err != nil {
		logrus.WithError(err).WithField("sub", sub).Error("MSC3861: failed to look up external ID")
		return err
	}

	if localpart == "" {
		// No mapping exists yet. Derive one from the introspected claims.
		localpart = deriveLocalpart(introspection, string(serverName))
		if localpart == "" {
			logrus.WithField("sub", sub).Warn("MSC3861: introspection response has no email, no username, no sub, and no existing external ID mapping")
			return nil
		}

		// Create the external ID mapping.
		if err := a.DB.CreateExternalIDMapping(ctx, localpart, serverName, issuer, sub); err != nil {
			logrus.WithError(err).WithFields(logrus.Fields{
				"localpart": localpart,
				"sub":       sub,
			}).Error("MSC3861: failed to create external ID mapping")
			return err
		}

		logrus.WithFields(logrus.Fields{
			"localpart": localpart,
			"sub":       sub,
		}).Info("MSC3861: created external ID mapping")
	}

	// 6. Validate the localpart before it is used for account provisioning.
	if err := zinternal.ValidateUsername(localpart, serverName); err != nil {
		logrus.WithError(err).WithField("localpart", localpart).Warn("MSC3861: localpart is not valid")
		return nil
	}

	// 7. Determine account type from scope.
	accountType := api.AccountTypeUser
	if strings.Contains(introspection.Scope, "urn:synapse:admin:*") {
		accountType = api.AccountTypeAdmin
	}

	// 8. Auto-provision user account if it doesn't exist.
	var createRes api.PerformAccountCreationResponse
	if err := a.PerformAccountCreation(ctx, &api.PerformAccountCreationRequest{
		AccountType: accountType,
		Localpart:   localpart,
		ServerName:  serverName,
		OnConflict:  api.ConflictUpdate,
	}, &createRes); err != nil {
		logrus.WithError(err).WithField("localpart", localpart).Error("MSC3861: failed to auto-provision user")
		return err
	}

	// 8. Extract device ID from scope and create a real device.
	deviceID := extractDeviceIDFromScope(introspection.Scope)
	if deviceID == "" {
		deviceID = "OIDC"
	}

	var deviceRes api.PerformDeviceCreationResponse
	if err := a.PerformDeviceCreation(ctx, &api.PerformDeviceCreationRequest{
		Localpart:          localpart,
		ServerName:         serverName,
		AccessToken:        req.AccessToken,
		DeviceID:           &deviceID,
		NoDeviceListUpdate: true,
	}, &deviceRes); err != nil {
		logrus.WithError(err).WithFields(logrus.Fields{
			"localpart": localpart,
			"device_id": deviceID,
		}).Error("MSC3861: failed to create/update device")
		return err
	}

	if deviceRes.Device != nil {
		deviceRes.Device.AccountType = accountType
		res.Device = deviceRes.Device
	}

	return nil
}
