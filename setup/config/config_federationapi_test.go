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
