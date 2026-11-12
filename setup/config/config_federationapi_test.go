package config

import (
	"net"
	"os"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/require"
)

func TestFederationAPIDefaultDenyNetworks(t *testing.T) {
	var c FederationAPI
	c.Defaults(DefaultOpts{})

	var denied []*net.IPNet
	for _, cidr := range c.DenyNetworkCIDRs {
		_, network, err := net.ParseCIDR(cidr)
		require.NoError(t, err, cidr)
		denied = append(denied, network)
	}
	isDenied := func(addr string) bool {
		ip := net.ParseIP(addr)
		require.NotNil(t, ip, addr)
		for _, network := range denied {
			if network.Contains(ip) {
				return true
			}
		}
		return false
	}

	for _, addr := range []string{
		"0.0.0.0", "0.1.2.3", // "this network", 0.0.0.0 reaches localhost
		"127.0.0.1", "10.0.0.5", "172.16.0.1", "192.168.1.1", "100.64.0.1", "169.254.169.254",
		"224.0.0.1", "239.255.255.250", // multicast
		"::",                                  // unspecified, reaches localhost
		"::1",                                 // loopback
		"fe80::1", "fe80:0:0:1::1", "febf::1", // whole link-local /10, not just /64
		"fec0::1",          // deprecated site-local
		"fd00::1",          // unique local
		"2002:a00:5::1",    // 6to4 wrapping 10.0.0.5
		"ff02::1",          // multicast
		"::ffff:127.0.0.1", // IPv4-mapped loopback
	} {
		require.True(t, isDenied(addr), "%s should be denied by default", addr)
	}

	for _, addr := range []string{
		"1.1.1.1", "8.8.8.8", "2001:4860:4860::8888", "2a01:4f8::1",
		// NAT64: on IPv6-only hosts IPv4-only servers resolve here.
		"64:ff9b::808:808",
	} {
		require.False(t, isDenied(addr), "%s should not be denied by default", addr)
	}
}

// The sample config documents the defaults, so the two must not drift apart.
func TestSampleConfigDenyNetworksMatchDefaults(t *testing.T) {
	data, err := os.ReadFile("../../zendrite-sample.yaml")
	require.NoError(t, err)
	var sample struct {
		FederationAPI struct {
			DenyNetworks  []string `yaml:"deny_networks"`
			AllowNetworks []string `yaml:"allow_networks"`
		} `yaml:"federation_api"`
	}
	require.NoError(t, yaml.Unmarshal(data, &sample))

	var c FederationAPI
	c.Defaults(DefaultOpts{})
	require.Equal(t, c.DenyNetworkCIDRs, sample.FederationAPI.DenyNetworks)
	require.Equal(t, c.AllowNetworkCIDRs, sample.FederationAPI.AllowNetworks)
}

func TestAllowNetworksBlockIPv6(t *testing.T) {
	var c FederationAPI
	c.Defaults(DefaultOpts{})
	require.False(t, allowNetworksBlockIPv6(c.AllowNetworkCIDRs), "the defaults allow IPv6")

	for _, tc := range []struct {
		cidrs []string
		want  bool
	}{
		{cidrs: []string{"0.0.0.0/0"}, want: true}, // the old Dendrite sample config
		{cidrs: []string{"0.0.0.0/0", "203.0.113.0/24"}, want: true},
		{cidrs: []string{"0.0.0.0/0", "::/0"}, want: false},
		{cidrs: []string{"2001:db8::/32"}, want: false},
		{cidrs: nil, want: false}, // empty allows everything
		{cidrs: []string{"0.0.0.0/0", "not-a-cidr"}, want: false},
	} {
		require.Equal(t, tc.want, allowNetworksBlockIPv6(tc.cidrs), tc.cidrs)
	}
}

func TestFederationAPIVerifyRejectsBadCIDRs(t *testing.T) {
	var c FederationAPI
	c.Defaults(DefaultOpts{Generate: true, SingleDatabase: true})
	c.Matrix = &Global{}
	c.Matrix.DatabaseOptions.ConnectionString = "file:test.db"

	var errs ConfigErrors
	c.Verify(&errs)
	require.Empty(t, errs, "the defaults are valid")

	c.AllowNetworkCIDRs = []string{"0.0.0.0/0", "::/0", "10.0.0.0/33"}
	c.DenyNetworkCIDRs = append(c.DenyNetworkCIDRs, "not-a-cidr")
	errs = nil
	c.Verify(&errs)
	require.Len(t, errs, 2)
	require.Contains(t, errs[0], "federation_api.allow_networks")
	require.Contains(t, errs[1], "federation_api.deny_networks")
}
