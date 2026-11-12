// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package config

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrustedProxiesDefaults(t *testing.T) {
	t.Parallel()
	var g Global
	g.Defaults(DefaultOpts{})
	assert.Equal(t, DefaultTrustedProxies, g.TrustedProxies)

	prefixes := g.TrustedProxyPrefixes()
	require.Len(t, prefixes, len(DefaultTrustedProxies))
	contains := func(ip string) bool {
		for _, p := range prefixes {
			if p.Contains(netip.MustParseAddr(ip)) {
				return true
			}
		}
		return false
	}
	for _, ip := range []string{"127.0.0.1", "::1", "10.1.2.3", "172.20.0.1", "192.168.1.1", "fd00::1"} {
		assert.True(t, contains(ip), ip)
	}
	for _, ip := range []string{"8.8.8.8", "172.32.0.1", "2001:db8::1"} {
		assert.False(t, contains(ip), ip)
	}
}

func TestTrustedProxiesVerify(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		proxies []string
		wantErr bool
	}{
		{name: "empty", proxies: []string{}},
		{name: "cidrs", proxies: []string{"10.0.0.0/8", "2001:db8::/32"}},
		{name: "single addresses", proxies: []string{"203.0.113.7", "2001:db8::1"}},
		{name: "bad cidr", proxies: []string{"10.0.0.0/33"}, wantErr: true},
		{name: "garbage", proxies: []string{"proxy.example.org"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := Global{TrustedProxies: tc.proxies}
			errs := &ConfigErrors{}
			g.Verify(errs)
			var found bool
			for _, e := range *errs {
				if strings.HasPrefix(e, "global.trusted_proxies") {
					found = true
				}
			}
			assert.Equal(t, tc.wantErr, found, "errors: %v", *errs)
		})
	}
}

func TestParseTrustedProxy(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"10.1.2.3/8":          "10.0.0.0/8",
		"203.0.113.7":         "203.0.113.7/32",
		"2001:db8::1":         "2001:db8::1/128",
		"::ffff:10.0.0.0/104": "10.0.0.0/8",
	} {
		p, err := ParseTrustedProxy(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, p.String(), in)
	}
}

func TestTrustedProxiesYAMLOverridesDefaults(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		yaml string
		want []string
	}{
		{name: "unset keeps defaults", yaml: "", want: DefaultTrustedProxies},
		{name: "list replaces defaults", yaml: "  trusted_proxies:\n  - 203.0.113.0/24\n", want: []string{"203.0.113.0/24"}},
		{name: "empty list trusts nobody", yaml: "  trusted_proxies: []\n", want: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data := strings.Replace(testConfig, "global:\n", "global:\n"+tc.yaml, 1)
			cfg, err := loadConfig("/my/config/dir", []byte(data), mockReadFile{
				"/my/config/dir/matrix_key.pem": testKey,
				"/my/config/dir/tls_cert.pem":   testCert,
			}.readFile)
			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg.Global.TrustedProxies)
		})
	}
}
