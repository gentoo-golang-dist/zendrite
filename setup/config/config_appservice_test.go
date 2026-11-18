// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAppServiceScopes covers the io.element.msc4502.scopes registration
// property (MSC4502).
func TestAppServiceScopes(t *testing.T) {
	t.Parallel()
	const base = `id: bridge
url: null
as_token: as_token
hs_token: hs_token
sender_localpart: _bridge
namespaces:
  users: []
  aliases: []
  rooms: []
`
	load := func(t *testing.T, registration string) (*Derived, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "registration.yaml")
		require.NoError(t, os.WriteFile(path, []byte(registration), 0o600))
		asAPI := &AppServiceAPI{
			Matrix:      &Global{ServerName: "test"},
			ConfigFiles: []string{path},
		}
		derived := &Derived{}
		return derived, loadAppServices(asAPI, derived)
	}

	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		derived, err := load(t, base)
		require.NoError(t, err)
		require.False(t, derived.ApplicationServices[0].HasScope(ScopeQueryRoomMembership))
	})

	t.Run("known scope", func(t *testing.T) {
		t.Parallel()
		derived, err := load(t, base+"io.element.msc4502.scopes: [\"urn:matrix:client:io.element.msc4502:rooms:is_joined\"]\n")
		require.NoError(t, err)
		require.True(t, derived.ApplicationServices[0].HasScope(ScopeQueryRoomMembership))
	})

	t.Run("unknown scope", func(t *testing.T) {
		t.Parallel()
		_, err := load(t, base+"io.element.msc4502.scopes: [\"does:not:exist\"]\n")
		require.ErrorContains(t, err, "does:not:exist")
	})

	t.Run("stable scope name is unknown", func(t *testing.T) {
		t.Parallel()
		_, err := load(t, base+"io.element.msc4502.scopes: [\"urn:matrix:client:rooms:is_joined\"]\n")
		require.Error(t, err)
	})

	t.Run("not a list", func(t *testing.T) {
		t.Parallel()
		_, err := load(t, base+"io.element.msc4502.scopes: not-a-list\n")
		require.Error(t, err)
	})
}

// TestAppServiceProxyConfig covers the io.element.msc4512.proxy_prefix and
// io.element.msc4512.proxy_url registration properties (MSC4512).
func TestAppServiceProxyConfig(t *testing.T) {
	t.Parallel()
	registration := func(id string, extra string) string {
		return "id: " + id + `
url: null
as_token: as_` + id + `
hs_token: hs_` + id + `
sender_localpart: _` + id + `
namespaces:
  users: []
` + extra
	}
	load := func(t *testing.T, registrations ...string) (*Derived, error) {
		t.Helper()
		dir := t.TempDir()
		asAPI := &AppServiceAPI{Matrix: &Global{ServerName: "test"}}
		for i, registration := range registrations {
			path := filepath.Join(dir, string(rune('a'+i))+".yaml")
			require.NoError(t, os.WriteFile(path, []byte(registration), 0o600))
			asAPI.ConfigFiles = append(asAPI.ConfigFiles, path)
		}
		derived := &Derived{}
		return derived, loadAppServices(asAPI, derived)
	}
	const livekit = "io.element.msc4512.proxy_prefix: rtc/livekit\nio.element.msc4512.proxy_url: http://127.0.0.1:1234/\n"

	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		derived, err := load(t, registration("lk", ""))
		require.NoError(t, err)
		as := derived.ApplicationServices[0]
		require.Nil(t, as.ProxyRules())
		require.Nil(t, as.ProxyHTTPClient)
	})

	t.Run("allowed prefix", func(t *testing.T) {
		t.Parallel()
		derived, err := load(t, registration("lk", "io.element.msc4512.proxy_prefix: /rtc/livekit/\nio.element.msc4512.proxy_url: https://lk.example.org/\n"))
		require.NoError(t, err)
		as := derived.ApplicationServices[0]
		require.Equal(t, "rtc/livekit", as.ProxyPrefix)
		require.Equal(t, "https://lk.example.org", as.ProxyURL)
		require.NotNil(t, as.ProxyRules())
		require.True(t, as.ProxyRules().AllowGuests)
		require.NotNil(t, as.ProxyHTTPClient)
	})

	for name, extra := range map[string]string{
		"prefix without url":    "io.element.msc4512.proxy_prefix: rtc/livekit\n",
		"url without prefix":    "io.element.msc4512.proxy_url: http://127.0.0.1:1234\n",
		"reserved prefix":       "io.element.msc4512.proxy_prefix: rooms\nio.element.msc4512.proxy_url: http://127.0.0.1:1234\n",
		"prefix of a prefix":    "io.element.msc4512.proxy_prefix: rtc\nio.element.msc4512.proxy_url: http://127.0.0.1:1234\n",
		"url without scheme":    "io.element.msc4512.proxy_prefix: rtc/livekit\nio.element.msc4512.proxy_url: 127.0.0.1:1234\n",
		"url with other scheme": "io.element.msc4512.proxy_prefix: rtc/livekit\nio.element.msc4512.proxy_url: unix:///run/lk.sock\n",
		"url with query":        "io.element.msc4512.proxy_prefix: rtc/livekit\nio.element.msc4512.proxy_url: http://127.0.0.1:1234/?a=b\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := load(t, registration("lk", extra))
			require.ErrorContains(t, err, "msc4512")
		})
	}

	t.Run("prefix claimed twice", func(t *testing.T) {
		t.Parallel()
		_, err := load(t, registration("one", livekit), registration("two", livekit))
		require.ErrorContains(t, err, "claim the same")
	})
}
