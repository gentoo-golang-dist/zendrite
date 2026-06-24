package config

import (
	"net/url"
	"strings"

	"github.com/sirupsen/logrus"
)

type MSCs struct {
	Matrix *Global `yaml:"-"`

	// The MSCs to enable. Supported MSCs include:
	// 'msc2444': Peeking over federation - https://github.com/matrix-org/matrix-doc/pull/2444
	// 'msc2753': Peeking via /sync - https://github.com/matrix-org/matrix-doc/pull/2753
	// 'msc2836': Threading - https://github.com/matrix-org/matrix-doc/pull/2836
	// 'msc3814': Dehydrated devices v2 - https://github.com/matrix-org/matrix-spec-proposals/pull/3814
	// 'msc3861': OIDC delegated authentication - https://github.com/matrix-org/matrix-spec-proposals/pull/3861
	// 'msc4115': Membership metadata on events - https://github.com/matrix-org/matrix-spec-proposals/pull/4115
	MSCs []string `yaml:"mscs"`

	Database DatabaseOptions `yaml:"database,omitempty"`

	// MSC3861 configuration for OIDC delegated authentication.
	MSC3861 MSC3861Config `yaml:"msc3861"`
}

// MSC3861Config holds configuration for delegated OIDC authentication via MSC3861.
type MSC3861Config struct {
	// Issuer is the OIDC provider URL (e.g. the Matrix Authentication Service URL).
	Issuer string `yaml:"issuer"`

	// ClientID is the OAuth 2.0 client ID used for token introspection.
	ClientID string `yaml:"client_id"`

	// ClientSecret is the OAuth 2.0 client secret used for token introspection.
	ClientSecret string `yaml:"client_secret"`

	// ClientAuthMethod is the authentication method used for introspection.
	// Supported values: "client_secret_basic" (default), "client_secret_post".
	ClientAuthMethod string `yaml:"client_auth_method"`

	// AdminToken is a static bearer token that grants admin-level access,
	// bypassing OIDC introspection. Useful for internal service-to-service calls.
	AdminToken string `yaml:"admin_token"`

	// AccountManagementURL is a URL to redirect users for account management
	// (e.g. password changes). Included in the well-known response.
	AccountManagementURL string `yaml:"account_management_url"`

	// IntrospectionEndpoint overrides the OIDC introspection endpoint URL.
	// If empty, defaults to {Issuer}/oauth2/introspect.
	IntrospectionEndpoint string `yaml:"introspection_endpoint"`

	// AuthorizationEndpoint overrides the OIDC authorization endpoint URL.
	// If empty, defaults to {Issuer}/oauth2/auth.
	AuthorizationEndpoint string `yaml:"authorization_endpoint"`

	// TokenEndpoint overrides the OIDC token endpoint URL.
	// If empty, defaults to {Issuer}/oauth2/token.
	TokenEndpoint string `yaml:"token_endpoint"`

	// RegistrationEndpoint overrides the OIDC dynamic client registration endpoint URL.
	// If empty, defaults to {Issuer}/oauth2/clients/register.
	RegistrationEndpoint string `yaml:"registration_endpoint"`

	// RevocationEndpoint overrides the OIDC token revocation endpoint URL.
	// If empty, defaults to {Issuer}/oauth2/revoke.
	RevocationEndpoint string `yaml:"revocation_endpoint"`

	// JWKSURI overrides the JWKS URI advertised in authorization server metadata.
	// If empty, defaults to {Issuer}/oauth2/keys.
	JWKSURI string `yaml:"jwks_uri"`
}

// msc3861Endpoint returns the configured endpoint or the default built from the issuer.
func (c *MSC3861Config) msc3861Endpoint(configured, defaultPath string) string {
	if configured != "" {
		return configured
	}
	return strings.TrimSuffix(c.Issuer, "/") + defaultPath
}

// AuthorizationEndpointOrDefault returns the authorization endpoint to advertise.
func (c *MSC3861Config) AuthorizationEndpointOrDefault() string {
	return c.msc3861Endpoint(c.AuthorizationEndpoint, "/oauth2/auth")
}

// TokenEndpointOrDefault returns the token endpoint to advertise.
func (c *MSC3861Config) TokenEndpointOrDefault() string {
	return c.msc3861Endpoint(c.TokenEndpoint, "/oauth2/token")
}

// RegistrationEndpointOrDefault returns the dynamic client registration endpoint to advertise.
func (c *MSC3861Config) RegistrationEndpointOrDefault() string {
	return c.msc3861Endpoint(c.RegistrationEndpoint, "/oauth2/clients/register")
}

// RevocationEndpointOrDefault returns the token revocation endpoint to advertise.
func (c *MSC3861Config) RevocationEndpointOrDefault() string {
	return c.msc3861Endpoint(c.RevocationEndpoint, "/oauth2/revoke")
}

// JWKSURIOrDefault returns the JWKS URI to advertise.
func (c *MSC3861Config) JWKSURIOrDefault() string {
	return c.msc3861Endpoint(c.JWKSURI, "/oauth2/keys")
}

// IntrospectionEndpointOrDefault returns the token introspection endpoint to use.
func (c *MSC3861Config) IntrospectionEndpointOrDefault() string {
	return c.msc3861Endpoint(c.IntrospectionEndpoint, "/oauth2/introspect")
}

func (c *MSCs) Defaults(opts DefaultOpts) {
	if opts.Generate {
		if !opts.SingleDatabase {
			c.Database.ConnectionString = "file:mscs.db"
		}
	}
}

// Enabled returns true if the given msc is enabled. Should in the form 'msc12345'.
func (c *MSCs) Enabled(msc string) bool {
	for _, m := range c.MSCs {
		if m == msc {
			return true
		}
	}
	return false
}

func (c *MSCs) Verify(configErrs *ConfigErrors) {
	if c.Matrix.DatabaseOptions.ConnectionString == "" {
		checkNotEmpty(configErrs, "mscs.database.connection_string", string(c.Database.ConnectionString))
	}
	if c.Enabled("msc3861") {
		checkNotEmpty(configErrs, "mscs.msc3861.issuer", c.MSC3861.Issuer)
		checkNotEmpty(configErrs, "mscs.msc3861.client_id", c.MSC3861.ClientID)
		checkNotEmpty(configErrs, "mscs.msc3861.client_secret", c.MSC3861.ClientSecret)

		if c.MSC3861.Issuer != "" {
			if u, err := url.Parse(c.MSC3861.Issuer); err != nil || u.Scheme == "" || u.Host == "" {
				configErrs.Add("mscs.msc3861.issuer must be a valid URL with scheme and host")
			}
		}

		if c.MSC3861.ClientAuthMethod != "" {
			switch c.MSC3861.ClientAuthMethod {
			case "client_secret_basic", "client_secret_post":
				// valid
			default:
				configErrs.Add("mscs.msc3861.client_auth_method must be 'client_secret_basic' or 'client_secret_post'")
			}
		}

		if c.MSC3861.AdminToken == "" {
			logrus.Warn("mscs.msc3861.admin_token is empty; MAS admin API endpoints will reject all requests")
		}
	}
}
