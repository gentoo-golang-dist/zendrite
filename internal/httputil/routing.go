// Copyright 2024 New Vector Ltd.
// Copyright 2020 The Matrix.org Foundation C.I.C.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package httputil

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/matrix-org/gomatrixserverlib/spec"
)

// URLDecodeMapValues is a function that iterates through each of the items in a
// map, URL decodes the value, and returns a new map with the decoded values
// under the same key names
func URLDecodeMapValues(vmap map[string]string) (map[string]string, error) {
	decoded := make(map[string]string, len(vmap))
	for key, value := range vmap {
		decodedVal, err := url.PathUnescape(value)
		if err != nil {
			return make(map[string]string), err
		}
		decoded[key] = decodedVal
	}

	return decoded, nil
}

// Vars extracts all path variables from an http.Request.
// This replaces mux.Vars() usage. It extracts variable names from the
// request's pattern and returns their values.
func Vars(r *http.Request) map[string]string {
	vars := make(map[string]string)
	pattern := r.Pattern
	if pattern == "" {
		return vars
	}
	// Extract variable names from the pattern
	// Pattern format is like "GET /path/{var1}/more/{var2}"
	// Remove the method prefix if present
	if idx := strings.Index(pattern, " "); idx != -1 {
		pattern = pattern[idx+1:]
	}
	// Find all {varName} patterns
	re := regexp.MustCompile(`\{([^}]+)\}`)
	matches := re.FindAllStringSubmatch(pattern, -1)
	for _, match := range matches {
		if len(match) >= 2 {
			varName := match[1]
			// Handle patterns like {var...} for wildcards
			varName = strings.TrimSuffix(varName, "...")
			vars[varName] = r.PathValue(varName)
		}
	}
	return vars
}

// Route represents a registered route that supports method chaining
type Route struct {
	router   *Router
	patterns []string // may have multiple patterns due to regex alternatives
	handler  http.Handler
}

// allHTTPMethods lists common HTTP methods used for 405 handling
var allHTTPMethods = []string{
	http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
	http.MethodPatch, http.MethodDelete, http.MethodOptions,
}

// tryRegisterHandler attempts to register a handler for a pattern, silently skipping
// if the pattern conflicts with an existing registration
func (r *Router) tryRegisterHandler(pattern string, handler http.Handler) (registered bool) {
	defer func() {
		if recover() != nil {
			// Pattern conflicted with existing registration, skip it
			registered = false
		}
	}()
	r.mux.Handle(pattern, handler)
	return true
}

// Methods registers the route for the specified HTTP methods
func (rt *Route) Methods(methods ...string) *Route {
	// Build a set of allowed methods
	allowedMethods := make(map[string]bool)
	for _, m := range methods {
		allowedMethods[m] = true
	}

	for _, pattern := range rt.patterns {
		fullPattern := rt.router.prefix + pattern
		for _, method := range allHTTPMethods {
			methodPattern := method + " " + fullPattern
			// Skip if already registered
			if rt.router.registeredOptions[methodPattern] {
				continue
			}

			var handler http.Handler
			if method == http.MethodOptions {
				// Always register OPTIONS for CORS
				handler = rt.handler
			} else if allowedMethods[method] {
				// Register the actual handler for allowed methods
				handler = rt.handler
			} else {
				// Register 405 handler for disallowed methods
				handler = rt.router.methodNotAllowedHandler
			}

			if rt.router.tryRegisterHandler(methodPattern, handler) {
				rt.router.registeredOptions[methodPattern] = true
			}
		}
	}
	return rt
}

// Name sets a name for the route (for compatibility with gorilla/mux).
// The name is not used with stdlib routing but is kept for API compatibility.
func (rt *Route) Name(_ string) *Route {
	return rt
}

// Router wraps http.ServeMux with path prefix support and additional features
type Router struct {
	mux                     *http.ServeMux
	prefix                  string
	notFoundHandler         http.Handler
	methodNotAllowedHandler http.Handler
	registeredOptions       map[string]bool // tracks OPTIONS patterns to avoid duplicates
}

// NewRouter creates a new Router with the given path prefix
func NewRouter(prefix string) *Router {
	return &Router{
		mux:                     http.NewServeMux(),
		prefix:                  strings.TrimSuffix(prefix, "/"),
		notFoundHandler:         NotFoundCORSHandler,
		methodNotAllowedHandler: NotAllowedHandler,
		registeredOptions:       make(map[string]bool),
	}
}

// normalisePattern converts gorilla/mux style patterns to Go 1.22 stdlib patterns.
// It removes regex patterns from path variables, e.g., {var:regex} becomes {var}
func normalisePattern(pattern string) string {
	re := regexp.MustCompile(`\{([^:}]+):[^}]+\}`)
	return re.ReplaceAllString(pattern, "{$1}")
}

// expandPattern converts gorilla/mux style patterns to Go 1.22 stdlib patterns.
// It handles regex alternatives like {var:(?:a|b)} by returning multiple patterns.
// Simple regex patterns like {var:[^/]+} are simplified to {var}.
func expandPattern(pattern string) []string {
	// Check for alternative patterns like {var:(?:opt1|opt2)}
	altRe := regexp.MustCompile(`\{[^:}]+:\(\?:([^)]+)\)\}`)
	if match := altRe.FindStringSubmatch(pattern); match != nil {
		alternatives := strings.Split(match[1], "|")
		var patterns []string
		for _, alt := range alternatives {
			// Replace the {var:(?:...)} with the literal alternative
			expanded := altRe.ReplaceAllString(pattern, alt)
			// Recursively expand in case there are more alternatives
			patterns = append(patterns, expandPattern(expanded)...)
		}
		return patterns
	}

	// No alternatives found, just normalise the pattern
	return []string{normalisePattern(pattern)}
}

// Handle registers a handler for the given pattern.
// Returns a Route for method chaining with .Methods()
// Pattern should not include the prefix - it will be added automatically.
func (r *Router) Handle(pattern string, handler http.Handler) *Route {
	return &Route{
		router:   r,
		patterns: expandPattern(pattern),
		handler:  handler,
	}
}

// HandleFunc registers a handler function for the given pattern.
// Returns a Route for method chaining with .Methods()
func (r *Router) HandleFunc(pattern string, f func(http.ResponseWriter, *http.Request)) *Route {
	return r.Handle(pattern, http.HandlerFunc(f))
}

// PathPrefix creates a subrouter with the given path prefix.
// This allows for hierarchical routing similar to gorilla/mux.
func (r *Router) PathPrefix(prefix string) *SubrouterBuilder {
	return &SubrouterBuilder{
		parent: r,
		prefix: normalisePattern(prefix),
	}
}

// SubrouterBuilder helps create subrouters
type SubrouterBuilder struct {
	parent *Router
	prefix string
}

// Subrouter creates a new Router that inherits the parent's prefix plus this prefix
func (sb *SubrouterBuilder) Subrouter() *Router {
	return &Router{
		mux:                     sb.parent.mux,
		prefix:                  sb.parent.prefix + strings.TrimSuffix(sb.prefix, "/"),
		notFoundHandler:         sb.parent.notFoundHandler,
		methodNotAllowedHandler: sb.parent.methodNotAllowedHandler,
		registeredOptions:       sb.parent.registeredOptions,
	}
}

// Handler registers a handler for this path prefix for common HTTP methods
func (sb *SubrouterBuilder) Handler(handler http.Handler) {
	pattern := sb.parent.prefix + sb.prefix
	// Use wildcard pattern for prefix matching
	if !strings.HasSuffix(pattern, "/") {
		pattern += "/"
	}
	// The {path...} wildcard matches empty string too, so it handles both
	// /prefix/ and /prefix/anything cases
	pattern += "{path...}"
	// Register for common HTTP methods to avoid conflicts with method-specific routes
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions} {
		sb.parent.mux.Handle(method+" "+pattern, handler)
	}
}

// HandlerFunc registers a handler function for this path prefix
func (sb *SubrouterBuilder) HandlerFunc(f func(http.ResponseWriter, *http.Request)) {
	sb.Handler(http.HandlerFunc(f))
}

// ServeHTTP implements http.Handler
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// Wrap response to detect 404s and use custom handler
	wrapped := &responseWriter404{ResponseWriter: w, router: r}
	r.mux.ServeHTTP(wrapped, req)
	// If we intercepted a 404 from stdlib mux (nothing else was written),
	// use our custom not found handler instead
	if wrapped.is404FromMux && !wrapped.written {
		r.notFoundHandler.ServeHTTP(w, req)
	}
}

// responseWriter404 wraps http.ResponseWriter to intercept stdlib's default 404 responses
type responseWriter404 struct {
	http.ResponseWriter
	router        *Router
	written       bool
	status        int
	is404FromMux  bool
	bufferedWrite []byte
}

func (w *responseWriter404) WriteHeader(code int) {
	w.status = code
	// If this is a 404, it might be from stdlib mux's default handler
	// Buffer it instead of forwarding immediately
	if code == http.StatusNotFound && !w.written {
		w.is404FromMux = true
		return
	}
	w.written = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseWriter404) Write(b []byte) (int, error) {
	// If we're buffering a potential stdlib 404, buffer the write too
	if w.is404FromMux && !w.written {
		w.bufferedWrite = append(w.bufferedWrite, b...)
		return len(b), nil
	}
	w.written = true
	return w.ResponseWriter.Write(b)
}

// SetNotFoundHandler sets the handler for 404 responses (for compatibility with gorilla/mux)
func (r *Router) SetNotFoundHandler(handler http.Handler) {
	r.notFoundHandler = handler
	// Register a catch-all handler for the prefix with wildcard
	pattern := r.prefix
	if pattern == "" {
		pattern = "/"
	}
	if !strings.HasSuffix(pattern, "/") {
		pattern += "/"
	}
	// Register for all common methods with a wildcard to catch unmatched paths
	wildcardPattern := pattern + "{notfound...}"
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
		r.mux.Handle(method+" "+wildcardPattern, handler)
	}
}

// SetMethodNotAllowedHandler sets the handler for 405 responses (for compatibility with gorilla/mux)
func (r *Router) SetMethodNotAllowedHandler(handler http.Handler) {
	r.methodNotAllowedHandler = handler
}

// Routers holds all the routers for different API paths
type Routers struct {
	Client        *Router
	Federation    *Router
	Keys          *Router
	Media         *Router
	WellKnown     *Router
	Static        *Router
	DendriteAdmin *Router
	SynapseAdmin  *Router
}

// NewRouters creates all routers with their respective path prefixes
func NewRouters() Routers {
	return Routers{
		Client:        NewRouter(PublicClientPathPrefix),
		Federation:    NewRouter(PublicFederationPathPrefix),
		Keys:          NewRouter(PublicKeyPathPrefix),
		Media:         NewRouter(PublicMediaPathPrefix),
		WellKnown:     NewRouter(PublicWellKnownPrefix),
		Static:        NewRouter(PublicStaticPath),
		DendriteAdmin: NewRouter(DendriteAdminPathPrefix),
		SynapseAdmin:  NewRouter(SynapseAdminPathPrefix),
	}
}

var NotAllowedHandler = WrapHandlerInCORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusMethodNotAllowed)
	unrecognizedErr, _ := json.Marshal(spec.Unrecognized("Unrecognized request")) // nolint:misspell
	_, _ = w.Write(unrecognizedErr)                                               // nolint:misspell
}))

var NotFoundCORSHandler = WrapHandlerInCORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	unrecognizedErr, _ := json.Marshal(spec.Unrecognized("Unrecognized request")) // nolint:misspell
	_, _ = w.Write(unrecognizedErr)                                               // nolint:misspell
}))
