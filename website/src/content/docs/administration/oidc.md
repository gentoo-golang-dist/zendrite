---
title: Delegated authentication (OIDC)
description: Delegate login, registration, and account management to an OpenID Connect provider such as Matrix Authentication Service
---

Zendrite can hand authentication to an external OpenID Connect (OIDC) provider instead of managing passwords itself.
This is the [MSC3861](https://github.com/matrix-org/matrix-spec-proposals/pull/3861) "next-generation auth" model that [Matrix Authentication Service (MAS)](https://github.com/element-hq/matrix-authentication-service) implements.

With delegation on, Zendrite stops being an identity store.
It validates each access token by asking the provider about it (RFC 7662 token introspection).
It creates local accounts and devices on demand from the answer.

The feature is opt-in and off by default.

## When to enable delegation

Enable it if you want single sign-on across services, or need OAuth 2.0 features such as centrally revocable sessions and per-device scopes.

On an existing server with password accounts, read [Migrating an existing server](#migrating-an-existing-server) first.
Zendrite doesn't link existing local accounts to OIDC identities automatically, and a wrong setup strands your users.

## Before you start

You need:

- A working OIDC provider that supports token introspection.
  MAS is the reference provider and the only one tested end to end.
- HTTPS on both the homeserver and the provider.
  Zendrite rejects plain HTTP issuers at startup unless the host is `localhost`, `127.0.0.1`, or `::1`.
- The public base URL of your homeserver, for example `https://matrix.example.com`.

## Step 1. Register Zendrite at the provider

Zendrite needs a confidential OAuth 2.0 client to introspect tokens.
With MAS, add it to the `clients` section of `config.yaml`:

```yaml
clients:
  - client_id: 0000000000000000000ZENDRITE
    client_auth_method: client_secret_basic
    client_secret: "a-long-random-secret"
    redirect_uris:
      - https://matrix.example.com/_matrix/client/v3/login/sso/callback
```

You only need the `redirect_uris` entry for the legacy SSO compatibility layer, which lets clients without native OIDC support log in.
See [Client support](#client-support).
The entry must exactly match the callback URL Zendrite advertises.
That URL is `{public_base_url}/_matrix/client/v3/login/sso/callback` unless you override it with `sso_callback_url`.

MAS also calls Zendrite's provisioning API to provision users and devices:

```yaml
matrix:
  kind: synapse
  homeserver: example.com
  endpoint: "http://localhost:8008"
  secret: "another-long-random-secret"
```

The `kind` **must be `synapse`** (the default in MAS 1.21+).
Do **not** use `synapse_legacy`.
It makes MAS call the legacy Synapse `/_synapse/admin/v1|v2` endpoints, which Zendrite doesn't implement.

Put the same `matrix.secret` value in Zendrite's `admin_token`.
Anyone who has it gets full admin access to the homeserver, so treat it like a root password.

## Step 2. Configure Zendrite

```yaml
mscs:
  mscs:
    - msc3861
  msc3861:
    issuer: "https://auth.example.com/"
    client_id: "0000000000000000000ZENDRITE"
    client_secret: "a-long-random-secret"
    admin_token: "another-long-random-secret"
    account_management_url: "https://auth.example.com/account"
    public_base_url: "https://matrix.example.com"
    sso_redirect_allowlist:
      - "https://app.example.com/"
      - "element://"
```

`issuer` must match, byte for byte, the `issuer` the provider advertises in its discovery document at `{issuer}/.well-known/openid-configuration`.
With MAS, that's `http.issuer`, which defaults to `http.public_base`.
If the two differ, Zendrite logs a warning, refuses to trust the document, and falls back to endpoint defaults derived from the config.

Zendrite discovers everything else from the provider.
You only need the endpoint overrides below if your provider publishes no discovery document, or one you need to deviate from.

### Configuration reference

| Field | Required | Description |
| --- | --- | --- |
| `issuer` | Yes | The OIDC provider URL. Must use `https`, except for loopback hosts during development. |
| `client_id` | Yes | OAuth 2.0 client ID registered with the provider. |
| `client_secret` | Yes | OAuth 2.0 client secret for introspection. |
| `client_auth_method` | No | `client_secret_basic` (default) or `client_secret_post`. Must match the client's registration at the provider. |
| `admin_token` | No | Static bearer token that grants admin access without introspection. Required for the MAS admin API and for confirmed cross-signing resets. |
| `account_management_url` | No | Where users manage their account. Advertised in `/.well-known/matrix/client` and as `account_management_uri` in the auth metadata. |
| `account_management_actions` | No | Actions the account management URL supports, for example `["org.matrix.profile", "org.matrix.devices_list"]`. |
| `public_base_url` | No | Public base URL of the homeserver. Zendrite builds client-facing URLs from it instead of the request's `Host` header. Strongly recommended behind a reverse proxy. |
| `sso_redirect_allowlist` | No | `redirectUrl` targets the legacy SSO flow accepts. See [Client support](#client-support). |
| `sso_callback_url` | No | Overrides the SSO callback URL. Only needed if it isn't `{public_base_url}/_matrix/client/v3/login/sso/callback`. |
| `introspection_endpoint` | No | Overrides discovery. Defaults to the discovered value, then `{issuer}/oauth2/introspect`. |
| `userinfo_endpoint` | No | Overrides discovery. Defaults to the discovered value, then `{issuer}/userinfo`. |
| `authorization_endpoint` | No | Overrides discovery. Defaults to the discovered value, then `{issuer}/oauth2/auth`. |
| `token_endpoint` | No | Overrides discovery. Defaults to the discovered value, then `{issuer}/oauth2/token`. |
| `registration_endpoint` | No | Overrides discovery. Defaults to the discovered value, then `{issuer}/oauth2/clients/register`. |
| `revocation_endpoint` | No | Overrides discovery. Defaults to the discovered value, then `{issuer}/oauth2/revoke`. |
| `device_authorization_endpoint` | No | Overrides discovery. Defaults to the discovered value, then `{issuer}/oauth2/device/auth`. |
| `end_session_endpoint` | No | Advertised to clients for RP-initiated logout. Defaults to the discovered value. |
| `jwks_uri` | No | Upstream JWKS to proxy. Defaults to the discovered value, then `{issuer}/oauth2/keys`. |

## Step 3. Configure the reverse proxy

Set `public_base_url`.
Without it, Zendrite builds client-facing URLs from the incoming `Host` and `X-Forwarded-Proto` headers.
A forged `Host` header can then poison the cached auth metadata Zendrite advertises.

Make sure your proxy sets `X-Forwarded-Proto` correctly and doesn't strip cookies on `/_matrix/client/v3/login/sso/*`.
The legacy SSO flow uses an `HttpOnly`, `SameSite=Lax` cookie to bind a login to the browser that started it.

## Step 4. Verify the setup

Restart Zendrite and check the discovery endpoints:

```sh
# Should list the issuer and the account management URL
curl -s https://matrix.example.com/.well-known/matrix/client | jq .m\\.authentication

# Should return the issuer
curl -s https://matrix.example.com/_matrix/client/v1/auth_issuer

# Should return the full authorization server metadata
curl -s https://matrix.example.com/_matrix/client/v1/auth_metadata

# Should return the provider's signing keys, proxied by Zendrite
curl -s https://matrix.example.com/_matrix/client/v1/auth_metadata/jwks
```

Zendrite proxies the provider's keys at the JWKS endpoint.
Browser clients can then fetch the keys without depending on the provider's CORS policy.
Zendrite caches responses for 15 minutes, and serves a stale copy if the provider becomes unreachable.

`/_matrix/client/versions` now advertises `org.matrix.msc2965` and `org.matrix.msc2967` in `unstable_features`.

## Client support

Clients with native OIDC support, such as Element X, use the metadata above and talk to the provider directly.
They need nothing else.
They can also sign in a new device by scanning a QR code, through the rendezvous sessions described under [QR code login](/mscs/#qr-code-login), which are on by default.

Clients without native support, such as current Element Web and Element Desktop, use the legacy SSO compatibility layer in Zendrite:

1. The client opens `/_matrix/client/v3/login/sso/redirect?redirectUrl=...`.
2. Zendrite starts a PKCE authorization-code flow against the provider and sets a browser-binding cookie.
3. The provider redirects back to `/_matrix/client/v3/login/sso/callback`.
4. Zendrite exchanges the code, provisions the user, and redirects to `redirectUrl` with a `loginToken`.
5. The client redeems the token through `POST /login` with `m.login.token`.
   It receives the access token, refresh token, and device ID issued by the provider.

**For a client hosted on a different origin, you must configure `sso_redirect_allowlist`.**
By default, Zendrite accepts only redirect targets on the homeserver's own origin, and rejects anything else with `M_INVALID_PARAM`.

This default is deliberate.
The login token passed to `redirectUrl` redeems into the access _and refresh_ tokens issued by the provider.
An unrestricted `redirectUrl` would let one crafted link send a user's long-lived provider credentials to whoever crafted it.
The browser-binding cookie doesn't help here, because the victim's own browser runs the whole flow and so has the cookie.

Zendrite matches entries as follows:

- `http` and `https` entries match on scheme, host, and path prefix at a path boundary.
  So `https://app.example.com` doesn't match `https://app.example.com.evil.com/`, a different port, or `https://app.example.com@evil.example.com/`.
- Entries with any other scheme, such as the `element://` desktop deep link, match as plain string prefixes.

Keep the list as short as you can.

## How accounts are provisioned

On the first token introspection for an unknown subject, Zendrite chooses a localpart.
It stores the mapping from the provider's `(issuer, sub)` to that localpart permanently.
A user's Matrix ID then stays stable even if they later change their email or display name at the provider.
How Zendrite chooses the localpart depends on whether MAS provisioning is on.

With `admin_token` set, the `username` claim is the localpart MAS already provisioned through `POST /_synapse/mas/provision_user`.
Zendrite uses it exactly as sent and attaches the identity to that account.
Zendrite rejects a username that isn't a valid Matrix localpart instead of rewriting it, because a rewritten name could resolve to another user's account.

Without `admin_token`, Zendrite derives a localpart in this order:

1. The `email` claim, but **only** when the provider also reports `email_verified: true`.
   If the domain matches your server name, Zendrite strips it, so `alice@example.com` becomes `@alice:example.com`.
2. The `username` claim.
3. The `sub` claim, for providers that expose no username.

Zendrite converts the result to lowercase and sanitises it to a valid Matrix localpart.
If an account already uses that localpart, Zendrite does **not** link the identity to it.
It falls back to a localpart derived from the `sub` claim, and refuses the login if that one is taken too.

Device IDs come from the token's `urn:matrix:client:device:<id>` scope.
Tokens without a device scope share a fallback device called `OIDC`.

Zendrite marks accounts whose token has the `urn:synapse:admin:*` scope as server admins.

## Admin access

Setting `admin_token` does two things.

First, it registers the MAS provisioning endpoints under `/_synapse/mas`, which MAS calls to manage users and devices (the same API Synapse 1.135+ implements):

- `GET /_synapse/mas/query_user?localpart=...`
- `POST /_synapse/mas/provision_user`
- `GET /_synapse/mas/is_localpart_available?localpart=...`
- `POST /_synapse/mas/upsert_device`
- `POST /_synapse/mas/update_device_display_name`
- `POST /_synapse/mas/delete_device`
- `POST /_synapse/mas/sync_devices`
- `POST /_synapse/mas/delete_user`
- `POST /_synapse/mas/reactivate_user`
- `POST /_synapse/mas/set_displayname`
- `POST /_synapse/mas/unset_displayname`
- `POST /_synapse/mas/allow_cross_signing_reset`

MAS authenticates to these with the admin token as a bearer token, not a user access token.
Configure MAS with `matrix.kind: synapse` (the default in MAS 1.21+) and `matrix.secret` set to Zendrite's `admin_token`.

Zendrite does **not** support the older `synapse_legacy` kind or the `/_synapse/admin/v1` user-management endpoints from previous Zendrite versions.
MAS 1.21+ must use the `synapse` kind with Zendrite.
If MAS logs 404 `M_UNRECOGNIZED` responses for `/_synapse/admin/v2/...` URLs, it runs in `synapse_legacy` mode.
Switch `matrix.kind` to `synapse`.

If `admin_token` is unset, Zendrite doesn't register these routes at all.

Second, clients can present the token itself as an access token, which resolves to an admin device for `@admin:{server_name}`.
Use this for service-to-service calls.

## Cross-signing resets

Replacing a user's cross-signing keys normally requires user-interactive auth, which can't be a password check under delegated authentication.
Zendrite uses the `m.oauth` UIA type and deep-links the user to the account management UI.

With `admin_token` configured, the reset only completes after the provider confirms it by calling `POST /_synapse/mas/allow_cross_signing_reset`.
The confirmation is single-use and valid for 10 minutes.

Without `admin_token`, the provider has no way to confirm anything.
Zendrite then accepts a resubmitted challenge and logs a warning.
This fallback is much weaker, because anyone with a valid access token can reset cross-signing keys.
Configure `admin_token`.

## What changes when this is enabled

| Area | Behaviour |
| --- | --- |
| `POST /login` | Accepts only `m.login.token`. `m.login.application_service` returns `400 M_APPSERVICE_LOGIN_UNSUPPORTED` (MSC4190). Everything else returns `403 M_FORBIDDEN`. |
| `GET /login` | Advertises `m.login.sso` (marked `oauth_aware_preferred`) and `m.login.token`. |
| `POST /register` | `403 M_FORBIDDEN`, except application-service registrations. These must set `inhibit_login: true` or get `400 M_APPSERVICE_LOGIN_UNSUPPORTED` (MSC4190). |
| `/register/available` | `403 M_FORBIDDEN`. |
| `/account/password`, `/account/deactivate` | `403 M_FORBIDDEN`. The provider handles these. |
| `/account/3pid/delete`, 3PID `requestToken` | `403 M_FORBIDDEN`. |
| `/logout`, `/logout/all`, `/delete_devices`, device modification | `403 M_FORBIDDEN`. The provider manages sessions. Application services still create, rename, and delete their users' devices with `PUT` and `DELETE /devices/{deviceId}` and `/delete_devices` (MSC4190). |
| `/auth/{authType}/fallback/web` | `403 M_FORBIDDEN`. |
| `GET /capabilities` | Reports `m.change_password` and `m.3pid_changes` as disabled. |
| `POST /refresh` | Bridges to the provider's refresh token grant. |
| Expired tokens | `401 M_UNKNOWN_TOKEN` with `soft_logout: true`, so clients refresh instead of logging out. |
| `/.well-known/matrix/client` | Gains an `m.authentication` section with the issuer and account management URL. |

Zendrite caches introspection results for 30 seconds, so a token revoked at the provider can keep working for up to 30 seconds.
It caches failed introspections for 5 seconds.

## Migrating an existing server

Enabling MSC3861 on a server with password accounts is a one-way change, and Zendrite doesn't link the two account types for you.

- Existing password sessions stop working.
  Every user has to log in again through the provider.
- Zendrite does **not** attach an existing local account to an OIDC identity that derives the same localpart.
  It deliberately refuses to attach an OIDC subject to an unrelated existing account, and falls back to a localpart derived from the subject.
  So `@alice:example.com` won't become the OIDC user `alice` unless you seed the mapping yourself.
- Zendrite refuses deactivated accounts instead of silently reactivating them for a matching token.

Plan the identity mapping before you turn this on, and test it on a staging server.

## High availability

Delegated authentication is qualified for [active-passive HA](/administration/high-availability/).
Zendrite writes everything it provisions through the fenced database connections of the active candidate:

- Accounts and devices created from a token's first request.
- The mapping from the provider's subject to a localpart.
- Login tokens.
- Changes made through the `/_synapse/mas/*` provisioning API.

A former owner can't write any of it once a successor has taken over.
Provisioning is idempotent, so retries don't create a second account, mapping, or device.
This covers MAS retrying a call against the successor, and a token's first request reaching the successor after the old owner already provisioned part of the account.

Zendrite keeps some state in process memory and loses it when the active candidate changes, as after any takeover:

| State | After a takeover |
| --- | --- |
| Token introspection results, cached for 30 seconds | The successor introspects each token again. Expect a burst of introspection requests to the provider. |
| Discovery and JWKS caches | Fetched again from the provider. |
| Legacy SSO sessions, and the provider tokens behind a login token not yet redeemed | `POST /login` with that login token answers `403`. The user starts the SSO login again. |
| A cross-signing reset confirmation from the provider not yet used | Zendrite challenges the upload again with `m.oauth`. The user confirms the reset in the account management UI again. |

Each of these fails closed.
A successor never honours a confirmation or login token it didn't issue, and a former owner refuses requests once it has been replaced.

All candidates must use the same `issuer` and `client_id`, and either all or none must set `admin_token`.
The HA deployment identity covers these values, so changing one requires an offline upgrade.
Rotating `client_secret` or `admin_token` doesn't.

## Known limitations

The legacy SSO compatibility layer keeps its in-flight state in process memory: SSO sessions, the login tokens the callback issues, and pending cross-signing reset confirmations.
So the `/login/sso/redirect`, `/login/sso/callback`, and `POST /login` requests of one login must all reach the same Zendrite process.
Active-passive HA meets this, because only the active candidate serves requests, except across a takeover (see [High availability](#high-availability)).
Several instances serving requests at the same time behind a load balancer break the flow, unless the balancer pins each client's requests to one instance.

Clients with native OIDC support are unaffected, because they never use the compatibility layer.

## Troubleshooting

**Every device shows up as `OIDC`.**
The token had no `urn:matrix:client:device:` scope.
Or introspection failed and Zendrite fell back to the UserInfo endpoint, which returns no scope.
Check the logs for an error about rejected client credentials.

**Logs show introspection falling back to userinfo.**
The provider rejected Zendrite's client credentials.
Check `client_id` and `client_secret`, and that `client_auth_method` matches the client's registration.
Authentication still works in this state, but device IDs and admin scope are lost, so don't leave it running like this.

**SSO login fails with `M_INVALID_PARAM` mentioning `sso_redirect_allowlist`.**
The client's `redirectUrl` isn't on the allowlist or on the homeserver's own origin.
See [Client support](#client-support).

**Logs warn that the discovered issuer doesn't match the configured issuer.**
`issuer` differs from what the provider advertises, often by a trailing slash or `http` versus `https`.
Zendrite falls back to endpoints derived from the config, which usually still work, but it skips discovery entirely.

**Clients are logged out instead of refreshing.**
Check that the provider returns a refresh token, and that the client supports `POST /refresh`.

**Admin endpoints return `403`.**
`admin_token` is unset, so Zendrite never registered the MAS admin routes.
