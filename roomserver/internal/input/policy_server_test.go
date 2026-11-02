// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package input

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"
)

func TestParsePolicyServerContent(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	key := spec.Base64Bytes(public).Encode()

	for _, tc := range []struct {
		name     string
		content  string
		unstable bool
		want     bool
	}{
		{name: "stable", content: `{"via":"policy.example","public_keys":{"ed25519":"` + key + `"}}`, want: true},
		{name: "padded key", content: `{"via":"policy.example","public_keys":{"ed25519":"` + key + `="}}`, want: true},
		{name: "unstable public_key", content: `{"via":"policy.example","public_key":"` + key + `"}`, unstable: true, want: true},
		{name: "public_key only counts when unstable", content: `{"via":"policy.example","public_key":"` + key + `"}`},
		{name: "unset", content: `{}`},
		{name: "no key", content: `{"via":"policy.example"}`},
		{name: "short key", content: `{"via":"policy.example","public_keys":{"ed25519":"c2hvcnQ"}}`},
		{name: "invalid server name", content: `{"via":"not a server","public_keys":{"ed25519":"` + key + `"}}`},
		{name: "not an object", content: `"policy.example"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := parsePolicyServerContent([]byte(tc.content), tc.unstable)
			require.Equal(t, tc.want, policy != nil)
			if tc.want {
				require.Equal(t, spec.ServerName("policy.example"), policy.via)
				require.Equal(t, public, policy.key)
			}
		})
	}
}

func TestPolicyServerCache(t *testing.T) {
	var cache policyServerCache
	cache.put("a", policyServerCacheEntry{signature: spec.Base64Bytes("sig")})
	entry, ok := cache.get("a")
	require.True(t, ok)
	require.Equal(t, spec.Base64Bytes("sig"), entry.signature)

	cache.entries["a"] = policyServerCacheEntry{expires: time.Now().Add(-time.Second)}
	_, ok = cache.get("a")
	require.False(t, ok, "expired entries are not served")

	for i := range policyServerCacheSize + 10 {
		cache.put(string(rune(i)), policyServerCacheEntry{})
	}
	require.LessOrEqual(t, len(cache.entries), policyServerCacheSize)
}
