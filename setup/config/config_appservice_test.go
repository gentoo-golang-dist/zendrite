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
