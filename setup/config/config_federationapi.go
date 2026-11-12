package config

import (
	"fmt"
	"net"
	"net/netip"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/sirupsen/logrus"
)

type FederationAPI struct {
	Matrix *Global `yaml:"-"`

	// The database stores information used by the federation destination queues to
	// send transactions to remote servers.
	Database DatabaseOptions `yaml:"database,omitempty"`

	// Federation failure threshold. How many consecutive failures that we should
	// tolerate when sending federation requests to a specific server. The backoff
	// is exponential with 2**(x+7) seconds, starting at ~4 minutes and capping at ~6 days:
	// 1 = 256s (~4min), 2 = 512s (~8min), ..., 12+ = 524288s (~6 days).
	// The default value is 11 if not specified, giving roughly 2 weeks of retry attempts
	// before the server is blacklisted.
	FederationMaxRetries uint32 `yaml:"send_max_retries"`

	// P2P Feature: Whether relaying to specific nodes should be enabled.
	// Defaults to false.
	// Note: Enabling relays introduces a huge startup delay, if you are not using
	// relays and have many servers to re-hydrate on start. Only enable this
	// if you are using relays!
	EnableRelays bool `yaml:"enable_relays"`

	// P2P Feature: How many consecutive failures that we should tolerate when
	// sending federation requests to a specific server until we should assume they
	// are offline. If we assume they are offline then we will attempt to send
	// messages to their relay server if we know of one that is appropriate.
	P2PFederationRetriesUntilAssumedOffline uint32 `yaml:"p2p_retries_until_assumed_offline"`

	// FederationDisableTLSValidation disables the validation of X.509 TLS certs
	// on remote federation endpoints. This is not recommended in production!
	DisableTLSValidation bool `yaml:"disable_tls_validation"`

	// DisableHTTPKeepalives prevents Zendrite from keeping HTTP connections
	// open for reuse for future requests. Connections will be closed quicker
	// but we may spend more time on TLS handshakes instead.
	DisableHTTPKeepalives bool `yaml:"disable_http_keepalives"`

	// Perspective keyservers, to use as a backup when direct key fetch
	// requests don't succeed
	KeyPerspectives KeyPerspectives `yaml:"key_perspectives"`

	// Should we prefer direct key fetches over perspective ones?
	PreferDirectFetch bool `yaml:"prefer_direct_fetch"`

	// Deny/Allow lists used for restricting request scopes.
	DenyNetworkCIDRs  []string `yaml:"deny_networks"`
	AllowNetworkCIDRs []string `yaml:"allow_networks"`
}

func (c *FederationAPI) Defaults(opts DefaultOpts) {
	c.FederationMaxRetries = 11
	c.P2PFederationRetriesUntilAssumedOffline = 1
	c.DisableTLSValidation = false
	c.DisableHTTPKeepalives = false
	// 64:ff9b::/96 (NAT64) is deliberately absent: on IPv6-only hosts behind
	// NAT64/DNS64 every IPv4-only remote server resolves into that prefix, so
	// denying it would break federation with them.
	c.DenyNetworkCIDRs = []string{
		"0.0.0.0/8",
		"127.0.0.1/8",
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"100.64.0.0/10",
		"169.254.0.0/16",
		"224.0.0.0/4",
		"::/128",
		"::1/128",
		"fe80::/10",
		"fec0::/10",
		"fc00::/7",
		"2002::/16",
		"ff00::/8",
	}
	c.AllowNetworkCIDRs = []string{
		"0.0.0.0/0",
		"::/0",
	}
	if opts.Generate {
		c.KeyPerspectives = KeyPerspectives{
			{
				ServerName: "matrix.org",
				Keys: []KeyPerspectiveTrustKey{
					{
						KeyID:     "ed25519:auto",
						PublicKey: "Noi6WqcDj0QmPxCNQqgezwTlBKrfqehY1u2FyWP9uYw",
					},
					{
						KeyID:     "ed25519:a_RXGa",
						PublicKey: "l8Hft5qXKn1vfHrg3p4+W8gELQVo8N13JkluMfmn2sQ",
					},
				},
			},
		}
		if !opts.SingleDatabase {
			c.Database.ConnectionString = "file:federationapi.db"
		}
	}
}

func (c *FederationAPI) Verify(configErrs *ConfigErrors) {
	if c.Matrix.DatabaseOptions.ConnectionString == "" {
		checkNotEmpty(configErrs, "federation_api.database.connection_string", string(c.Database.ConnectionString))
	}
	// The federation client skips CIDRs it can't parse, which quietly weakens
	// the deny list, so refuse to start on one instead.
	for _, cidr := range c.AllowNetworkCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			configErrs.Add(fmt.Sprintf("federation_api.allow_networks: %q is not a valid CIDR range", cidr))
		}
	}
	for _, cidr := range c.DenyNetworkCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			configErrs.Add(fmt.Sprintf("federation_api.deny_networks: %q is not a valid CIDR range", cidr))
		}
	}
	if allowNetworksBlockIPv6(c.AllowNetworkCIDRs) {
		logrus.Warn("federation_api.allow_networks only contains IPv4 ranges, so outgoing federation to IPv6 addresses is refused and IPv6-only servers are unreachable. " +
			"Configs from Dendrite or Zendrite before v3.0.0 often list only 0.0.0.0/0: add '::/0' to allow IPv6")
	}
}

// allowNetworksBlockIPv6 reports whether an allow list restricts outgoing
// federation to IPv4. An empty list allows everything, so it doesn't.
func allowNetworksBlockIPv6(cidrs []string) bool {
	if len(cidrs) == 0 {
		return false
	}
	for _, cidr := range cidrs {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil || !prefix.Addr().Is4() {
			// An IPv6 range, or an entry we can't judge: don't warn.
			return false
		}
	}
	return true
}

// The config for setting a proxy to use for server->server requests.
type Proxy struct {
	// Is the proxy enabled?
	Enabled bool `yaml:"enabled"`
	// The protocol for the proxy (http / https / socks5)
	Protocol string `yaml:"protocol"`
	// The host where the proxy is listening
	Host string `yaml:"host"`
	// The port on which the proxy is listening
	Port uint16 `yaml:"port"`
}

func (c *Proxy) Defaults() {
	c.Enabled = false
	c.Protocol = "http"
	c.Host = "localhost"
	c.Port = 8080
}

func (c *Proxy) Verify(configErrs *ConfigErrors) {
}

// KeyPerspectives are used to configure perspective key servers for
// retrieving server keys.
type KeyPerspectives []KeyPerspective

type KeyPerspective struct {
	// The server name of the perspective key server
	ServerName spec.ServerName `yaml:"server_name"`
	// Server keys for the perspective user, used to verify the
	// keys have been signed by the perspective server
	Keys []KeyPerspectiveTrustKey `yaml:"keys"`
}

type KeyPerspectiveTrustKey struct {
	// The key ID, e.g. ed25519:auto
	KeyID gomatrixserverlib.KeyID `yaml:"key_id"`
	// The public key in base64 unpadded format
	PublicKey string `yaml:"public_key"`
}
