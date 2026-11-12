package httputil

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/setup/config"
)

func TestClientAddr(t *testing.T) {
	trusted := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("fd00::/8"),
	}
	for _, tc := range []struct {
		name       string
		remoteAddr string
		xff        []string
		realIP     string
		want       string
	}{
		{name: "direct client", remoteAddr: "203.0.113.5:1234", want: "203.0.113.5"},
		{name: "spoofed XFF from untrusted peer is ignored", remoteAddr: "203.0.113.5:1234", xff: []string{"1.2.3.4"}, want: "203.0.113.5"},
		{name: "spoofed X-Real-IP from untrusted peer is ignored", remoteAddr: "203.0.113.5:1234", realIP: "1.2.3.4", want: "203.0.113.5"},
		{name: "XFF from trusted proxy", remoteAddr: "10.0.0.2:1234", xff: []string{"198.51.100.7"}, want: "198.51.100.7"},
		{name: "rightmost untrusted hop wins over client-supplied entries", remoteAddr: "10.0.0.2:1234", xff: []string{"1.2.3.4, 198.51.100.7"}, want: "198.51.100.7"},
		{name: "trusted hops are skipped", remoteAddr: "10.0.0.2:1234", xff: []string{"1.2.3.4, 198.51.100.7, 10.0.0.9"}, want: "198.51.100.7"},
		{name: "multiple XFF headers are joined", remoteAddr: "10.0.0.2:1234", xff: []string{"1.2.3.4", "198.51.100.7, 10.0.0.9"}, want: "198.51.100.7"},
		{name: "all hops trusted picks leftmost", remoteAddr: "10.0.0.2:1234", xff: []string{"10.0.0.8, 10.0.0.9"}, want: "10.0.0.8"},
		{name: "garbage hop stops the walk", remoteAddr: "10.0.0.2:1234", xff: []string{"198.51.100.7, nonsense"}, want: "10.0.0.2"},
		{name: "X-Real-IP from trusted proxy", remoteAddr: "10.0.0.2:1234", realIP: "198.51.100.7", want: "198.51.100.7"},
		{name: "XFF preferred over X-Real-IP", remoteAddr: "10.0.0.2:1234", xff: []string{"198.51.100.7"}, realIP: "198.51.100.8", want: "198.51.100.7"},
		{name: "trusted proxy without headers", remoteAddr: "10.0.0.2:1234", want: "10.0.0.2"},
		{name: "IPv6 peer", remoteAddr: "[2001:db8::1]:1234", xff: []string{"1.2.3.4"}, want: "2001:db8::1"},
		{name: "IPv6 trusted proxy", remoteAddr: "[fd00::2]:1234", xff: []string{"2001:db8::7"}, want: "2001:db8::7"},
		{name: "IPv4-mapped peer is unmapped", remoteAddr: "[::ffff:10.0.0.2]:1234", xff: []string{"198.51.100.7"}, want: "198.51.100.7"},
		{name: "XFF hop with port", remoteAddr: "10.0.0.2:1234", xff: []string{"[2001:db8::7]:443"}, want: "2001:db8::7"},
		{name: "unix socket peer is trusted", remoteAddr: "@", xff: []string{"198.51.100.7"}, want: "198.51.100.7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.remoteAddr
			for _, v := range tc.xff {
				req.Header.Add("X-Forwarded-For", v)
			}
			if tc.realIP != "" {
				req.Header.Set("X-Real-IP", tc.realIP)
			}
			got := clientAddr(req, trusted)
			require.True(t, got.IsValid())
			assert.Equal(t, tc.want, got.String())
		})
	}
}

func TestClientIPDefaultTrust(t *testing.T) {
	// The package default must match config.DefaultTrustedProxies so a
	// proxy on loopback or a private network keeps working out of the box.
	require.Len(t, currentTrustedProxies(), len(config.DefaultTrustedProxies))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	assert.Equal(t, "198.51.100.7", ClientIP(req))

	req.RemoteAddr = "203.0.113.5:5555"
	assert.Equal(t, "203.0.113.5", ClientIP(req))
}

func TestRateLimitKey(t *testing.T) {
	for in, want := range map[string]string{
		"198.51.100.7":           "198.51.100.7",
		"::ffff:198.51.100.7":    "198.51.100.7",
		"2001:db8:1:2:3:4:5:6":   "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff::1":   "2001:db8:1:2::/64",
		"2001:db8:1:3::1":        "2001:db8:1:3::/64",
		"not-an-ip-unix-socket@": "not-an-ip-unix-socket@",
	} {
		assert.Equal(t, want, rateLimitKey(in), in)
	}
}

func TestRateLimitIgnoresSpoofedXFFAndBucketsIPv6(t *testing.T) {
	limit := func(l *RateLimits, remoteAddr, xff string) bool {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = remoteAddr
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		return l.Limit(req, nil) == nil
	}
	newLimiter := func(ctx context.Context) *RateLimits {
		// A long cooloff keeps slots occupied for the whole test.
		return NewRateLimits(&config.RateLimiting{Enabled: true, Threshold: 1, CooloffMS: 60_000}, ctx) //nolint:contextcheck
	}

	t.Run("random XFF from public client does not reset the limit", func(t *testing.T) {
		l := newLimiter(t.Context())
		require.True(t, limit(l, "203.0.113.5:1000", "1.1.1.1"))
		require.False(t, limit(l, "203.0.113.5:1001", "2.2.2.2"))
	})

	t.Run("clients behind a trusted proxy are limited separately", func(t *testing.T) {
		l := newLimiter(t.Context())
		require.True(t, limit(l, "127.0.0.1:1000", "198.51.100.1"))
		require.True(t, limit(l, "127.0.0.1:1001", "198.51.100.2"))
		require.False(t, limit(l, "127.0.0.1:1002", "198.51.100.1"))
	})

	t.Run("IPv6 addresses in one /64 share a bucket", func(t *testing.T) {
		l := newLimiter(t.Context())
		require.True(t, limit(l, "[2001:db8:0:1::1]:1000", ""))
		require.False(t, limit(l, "[2001:db8:0:1::dead:beef]:1000", ""))
		require.True(t, limit(l, "[2001:db8:0:2::1]:1000", ""))
	})
}
