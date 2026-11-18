// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

// Package appserviceproxy forwards the parts of the client-server and
// federation APIs that application services claim to them (MSC4512).
package appserviceproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
	"golang.org/x/time/rate"

	"codefloe.com/pat-s/zendrite/internal/httputil"
	"codefloe.com/pat-s/zendrite/setup/config"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// The headers that tell the application service who made a proxied request.
const (
	UserIDHeader = "X-Matrix-User-Identifier"
	OriginHeader = "X-Matrix-Origin"
)

// The APIs whose paths an application service can claim.
const (
	ClientAPI     = "client"
	FederationAPI = "federation"
)

// Federation rate limit per origin server for proxied requests. Synapse
// applies its federation rate limiter, which is per origin as well.
const (
	federationRequestsPerSecond = 10
	federationRequestBurst      = 50
)

// forwardedRequestHeaders are the request headers an application service
// could need. Every other one, including Authorization, is dropped, as in
// Synapse.
var forwardedRequestHeaders = []string{"Content-Type", "Accept", "Accept-Language"}

// hopByHopHeaders are the hop-by-hop headers of RFC 2616 section 13.5.1,
// which are not copied from the application service's response.
var hopByHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Trailers":            {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

// proxiedMethods are the methods that are forwarded, as in Synapse. Other
// methods are left to the routes. Client requests also take OPTIONS, which
// answers CORS preflights.
var proxiedMethods = map[string]struct{}{
	http.MethodGet: {}, http.MethodPost: {}, http.MethodPut: {}, http.MethodDelete: {},
}

// maxClientBodyBytes bounds the body of a proxied client request, which is
// streamed to the application service. The endpoints that may be claimed
// take small JSON bodies.
const maxClientBodyBytes = 10 << 20

// maxOrigins bounds how many origins the federation rate limiter tracks at
// once; it starts afresh when more have been seen.
const maxOrigins = 10000

// PathPattern matches the paths under prefix in the given API, after a
// version segment of v1, v3 and so on, or unstable/<namespace>.
func PathPattern(api, prefix string) *regexp.Regexp {
	return regexp.MustCompile(`^/_matrix/` + api + `/(?:unstable/[^/]+|v[^/]+)/` + regexp.QuoteMeta(prefix) + `(?:/.*)?$`)
}

// HasDotSegments reports whether a percent-encoded path has a "." or ".."
// segment once decoded, or cannot be decoded. Such a path is refused rather
// than resolved, since the signature of a federation request covers the path
// as it was sent.
func HasDotSegments(escapedPath string) bool {
	path, err := url.PathUnescape(escapedPath)
	if err != nil {
		return true
	}
	for segment := range strings.SplitSeq(path, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

// Claims reports whether an application service serves the given prefix.
func Claims(appservices []config.ApplicationService, prefix string) bool {
	for i := range appservices {
		if appservices[i].ProxyRules() != nil && appservices[i].ProxyHTTPClient != nil && appservices[i].ProxyPrefix == prefix {
			return true
		}
	}
	return false
}

// route is an application service and the paths it claims in one API.
type route struct {
	as      *config.ApplicationService
	pattern *regexp.Regexp
}

// routes returns the application services that claim a prefix in api.
func routes(appservices []config.ApplicationService, api string) []route {
	var claimed []route
	for i := range appservices {
		as := &appservices[i]
		if as.ProxyRules() == nil || as.ProxyHTTPClient == nil {
			continue
		}
		claimed = append(claimed, route{as: as, pattern: PathPattern(api, as.ProxyPrefix)})
	}
	return claimed
}

// interceptor returns the router hook that sends the requests under a claimed
// prefix to handler.
func interceptor(claimed []route, preflight bool, handler func(*config.ApplicationService) http.Handler) httputil.Interceptor {
	handlers := make([]http.Handler, len(claimed))
	for i := range claimed {
		handlers[i] = handler(claimed[i].as)
	}
	return func(req *http.Request) http.Handler {
		if _, ok := proxiedMethods[req.Method]; !ok && (!preflight || req.Method != http.MethodOptions) {
			return nil
		}
		path := req.URL.EscapedPath()
		for i := range claimed {
			if claimed[i].pattern.MatchString(path) {
				return handlers[i]
			}
		}
		return nil
	}
}

// RegisterClient forwards the client-server requests under the claimed
// prefixes to their application services. The requests are authenticated
// and rate limited first. It takes over routes Zendrite registers under the
// same prefix, such as its own LiveKit token endpoint.
func RegisterClient(
	router *httputil.Router, appservices []config.ApplicationService,
	userAPI userapi.QueryAcccessTokenAPI, rateLimits *httputil.RateLimits, legacyAuth bool,
) []*config.ApplicationService {
	claimed := routes(appservices, ClientAPI)
	if len(claimed) == 0 {
		return nil
	}
	router.Intercept(interceptor(claimed, true, func(as *config.ApplicationService) http.Handler {
		var checks []httputil.AuthAPIOption
		if as.ProxyRules().AllowGuests {
			checks = append(checks, httputil.WithAllowGuests(), httputil.WithAllowCallGuests())
		}
		return clientHandler(as, userAPI, rateLimits, legacyAuth, checks)
	}))
	return claimedServices(claimed)
}

// RegisterFederation forwards the federation requests under the claimed
// prefixes to their application services, once their signature is verified.
func RegisterFederation(
	router *httputil.Router, appservices []config.ApplicationService,
	serverName spec.ServerName, isLocalServerName func(spec.ServerName) bool,
	keys gomatrixserverlib.JSONVerifier, wakeup func(context.Context, spec.ServerName), legacyAuth bool,
) []*config.ApplicationService {
	claimed := routes(appservices, FederationAPI)
	if len(claimed) == 0 {
		return nil
	}
	limiter := newOriginLimiter()
	router.Intercept(interceptor(claimed, false, func(as *config.ApplicationService) http.Handler {
		return federationHandler(as, serverName, isLocalServerName, keys, wakeup, limiter, legacyAuth)
	}))
	return claimedServices(claimed)
}

func claimedServices(claimed []route) []*config.ApplicationService {
	services := make([]*config.ApplicationService, len(claimed))
	for i := range claimed {
		services[i] = claimed[i].as
	}
	return services
}

func clientHandler(
	as *config.ApplicationService, userAPI userapi.QueryAcccessTokenAPI,
	rateLimits *httputil.RateLimits, legacyAuth bool, checks []httputil.AuthAPIOption,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodOptions {
			util.SetCORSHeaders(w)
			w.WriteHeader(http.StatusOK)
			return
		}
		if HasDotSegments(req.URL.EscapedPath()) {
			writeJSON(w, http.StatusBadRequest, spec.InvalidParam("Request path is not allowed"))
			return
		}
		device, errRes := httputil.VerifyDevice(req, userAPI, checks...)
		if errRes != nil {
			writeJSONResponse(w, errRes)
			return
		}
		if errRes = rateLimits.Limit(req, device); errRes != nil {
			writeJSONResponse(w, errRes)
			return
		}
		if req.ContentLength > maxClientBodyBytes {
			writeJSON(w, http.StatusRequestEntityTooLarge, spec.TooLarge("The request body is too large"))
			return
		}
		var body io.Reader
		if req.ContentLength != 0 && req.Body != nil {
			body = http.MaxBytesReader(w, req.Body, maxClientBodyBytes)
		}
		forward(w, req, as, legacyAuth, body, req.ContentLength, UserIDHeader, device.UserID)
	})
}

func federationHandler(
	as *config.ApplicationService, serverName spec.ServerName, isLocalServerName func(spec.ServerName) bool,
	keys gomatrixserverlib.JSONVerifier, wakeup func(context.Context, spec.ServerName), limiter *originLimiter, legacyAuth bool,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if HasDotSegments(req.URL.EscapedPath()) {
			writeJSON(w, http.StatusBadRequest, spec.InvalidParam("Request path is not allowed"))
			return
		}
		fedReq, errResp := fclient.VerifyHTTPRequest(req, time.Now(), serverName, isLocalServerName, keys)
		if fedReq == nil {
			for name, value := range errResp.Headers {
				w.Header().Set(name, value)
			}
			writeJSON(w, errResp.Code, errResp.JSON)
			return
		}
		origin := fedReq.Origin()
		if !limiter.allow(origin) {
			writeJSON(w, http.StatusTooManyRequests, spec.LimitExceeded("Too many requests", time.Second.Milliseconds()))
			return
		}
		if wakeup != nil {
			go wakeup(context.WithoutCancel(req.Context()), origin)
		}
		var body io.Reader
		content := fedReq.Content()
		if len(content) > 0 {
			body = bytes.NewReader(content)
		}
		forward(w, req, as, legacyAuth, body, int64(len(content)), OriginHeader, string(origin))
	})
}

// forward sends the request to the application service's proxy URL and
// streams its response back.
func forward(
	w http.ResponseWriter, req *http.Request, as *config.ApplicationService, legacyAuth bool,
	body io.Reader, contentLength int64, identityHeader, identity string,
) {
	target := as.ProxyURL + req.URL.EscapedPath()
	if rawQuery := proxiedQuery(req.URL.RawQuery, as.HSToken, legacyAuth); rawQuery != "" {
		target += "?" + rawQuery
	}

	outReq, err := http.NewRequestWithContext(req.Context(), req.Method, target, body)
	if err != nil {
		util.GetLogger(req.Context()).WithError(err).WithField("appservice_id", as.ID).Error("Building the proxied request failed")
		writeJSON(w, http.StatusInternalServerError, spec.InternalServerError{})
		return
	}
	if body != nil && contentLength > 0 {
		outReq.ContentLength = contentLength
	}
	for _, name := range forwardedRequestHeaders {
		if values := req.Header.Values(name); len(values) > 0 {
			outReq.Header[name] = values
		}
	}
	outReq.Header.Set("Authorization", "Bearer "+as.HSToken)
	outReq.Header.Set(identityHeader, identity)

	resp, err := as.ProxyHTTPClient.Do(outReq)
	if err != nil {
		util.GetLogger(req.Context()).WithError(err).WithField("appservice_id", as.ID).Warn("Proxying a request to the application service failed")
		writeJSON(w, http.StatusBadGateway, spec.Unknown("The application service could not be reached"))
		return
	}
	defer resp.Body.Close()

	dropped := make(map[string]struct{}, len(hopByHopHeaders))
	for name := range hopByHopHeaders {
		dropped[name] = struct{}{}
	}
	for _, value := range resp.Header.Values("Connection") {
		for name := range strings.SplitSeq(value, ",") {
			dropped[http.CanonicalHeaderKey(strings.TrimSpace(name))] = struct{}{}
		}
	}
	util.SetCORSHeaders(w)
	for name, values := range resp.Header {
		if _, ok := dropped[name]; ok {
			continue
		}
		w.Header()[name] = values
	}
	w.WriteHeader(resp.StatusCode)
	copyFlushing(w, resp.Body)
}

// proxiedQuery is the query string sent to the application service: the
// requester's own, without an access token in any case, and with the
// hs_token when legacy_auth asks for it. It is passed on verbatim when there
// is nothing to change.
func proxiedQuery(rawQuery, hsToken string, legacyAuth bool) string {
	query, err := url.ParseQuery(rawQuery)
	hasToken := false
	for key := range query {
		if strings.EqualFold(key, "access_token") {
			hasToken = true
			query.Del(key)
		}
	}
	if !hasToken && !legacyAuth && err == nil {
		return rawQuery
	}
	if legacyAuth {
		query.Set("access_token", hsToken)
	}
	return query.Encode()
}

// copyFlushing streams the response body, flushing after every read so that
// a long-lived response reaches the requester as it is produced.
func copyFlushing(w http.ResponseWriter, body io.Reader) {
	controller := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			_ = controller.Flush()
		}
		if err != nil {
			return
		}
	}
}

func writeJSONResponse(w http.ResponseWriter, res *util.JSONResponse) {
	for name, value := range res.Headers {
		w.Header().Set(name, value)
	}
	writeJSON(w, res.Code, res.JSON)
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	util.SetCORSHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		return
	}
}

// originLimiter rate limits proxied federation requests per origin server.
type originLimiter struct {
	mu       sync.Mutex
	limiters map[spec.ServerName]*rate.Limiter
}

func newOriginLimiter() *originLimiter {
	return &originLimiter{limiters: make(map[spec.ServerName]*rate.Limiter)}
}

func (l *originLimiter) allow(origin spec.ServerName) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	limiter, ok := l.limiters[origin]
	if !ok {
		if len(l.limiters) >= maxOrigins {
			clear(l.limiters)
		}
		limiter = rate.NewLimiter(federationRequestsPerSecond, federationRequestBurst)
		l.limiters[origin] = limiter
	}
	return limiter.Allow()
}
