package httputil

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestExpandPattern(t *testing.T) {
	tests := []struct {
		name     string
		pattern  string
		expected []string
	}{
		{
			name:     "simple pattern",
			pattern:  "/rooms/{roomID}/state",
			expected: []string{"/rooms/{roomID}/state"},
		},
		{
			name:     "regex stripped",
			pattern:  "/rooms/{roomID}/state/{eventType:[^/]+}",
			expected: []string{"/rooms/{roomID}/state/{eventType}"},
		},
		{
			name:     "optional trailing slash produces two patterns",
			pattern:  "/rooms/{roomID}/state/{eventType:[^/]+/?}",
			expected: []string{"/rooms/{roomID}/state/{eventType}", "/rooms/{roomID}/state/{eventType}/"},
		},
		{
			name:     "alternatives produce multiple patterns",
			pattern:  "/{apiversion:(?:r0|v3)}/rooms/{roomID}",
			expected: []string{"/r0/rooms/{roomID}", "/v3/rooms/{roomID}"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := expandPattern(tc.pattern)
			if len(got) != len(tc.expected) {
				t.Fatalf("expected %d patterns, got %d: %v", len(tc.expected), len(got), got)
			}
			for i := range tc.expected {
				if got[i] != tc.expected[i] {
					t.Errorf("pattern[%d]: expected %q, got %q", i, tc.expected[i], got[i])
				}
			}
		})
	}
}

func TestOptionalTrailingSlashRouting(t *testing.T) {
	router := NewRouter("/_matrix/client")
	handled := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handled = true
		w.WriteHeader(http.StatusOK)
	})

	router.Handle("/rooms/{roomID}/state/{eventType:[^/]+/?}", handler).
		Methods(http.MethodPut, http.MethodOptions)

	// Test without trailing slash
	handled = false
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/_matrix/client/rooms/!foo:bar/state/m.room.avatar", nil)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !handled {
		t.Fatalf("without trailing slash: expected 200, got %d (handled=%v)", rec.Code, handled)
	}

	// Test with trailing slash
	handled = false
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/_matrix/client/rooms/!foo:bar/state/m.room.avatar/", nil)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !handled {
		t.Fatalf("with trailing slash: expected 200, got %d (handled=%v)", rec.Code, handled)
	}

	// Test OPTIONS with trailing slash
	handled = false
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodOptions, "/_matrix/client/rooms/!foo:bar/state/m.room.avatar/", nil)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !handled {
		t.Fatalf("OPTIONS with trailing slash: expected 200, got %d (handled=%v)", rec.Code, handled)
	}
}

func TestRoutersError(t *testing.T) {
	r := NewRouters()

	// not found test
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, filepath.Join(PublicFederationPathPrefix, "doesnotexist"), nil)
	r.Federation.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unexpected status code: %d - %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Result().Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("unexpected content-type: %s", ct)
	}

	// not allowed test
	r.DendriteAdmin.
		Handle("/test", http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {})).
		Methods(http.MethodPost)

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, filepath.Join(DendriteAdminPathPrefix, "test"), nil)
	r.DendriteAdmin.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unexpected status code: %d - %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Result().Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("unexpected content-type: %s", ct)
	}
}
