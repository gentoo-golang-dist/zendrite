---
title: MSC Support
description: Implementation status of Matrix Spec Changes (MSCs) in Zendrite.
---

[MSCs](https://spec.matrix.org/proposals/) (Matrix Spec Changes) are proposals to extend or modify the Matrix protocol.
This page tracks the implementation status of notable MSCs in Zendrite.

Many MSCs listed here have since been merged into the [Matrix specification](https://spec.matrix.org/) proper.
They are listed by their original MSC number and the first spec version which includes them.

## Sync

| MSC | Title | Spec | Status |
| --- | --- | --- | --- |
| [MSC4186](https://github.com/matrix-org/matrix-spec-proposals/pull/4186) | Simplified Sliding Sync | N/A | Implemented (native, no proxy needed) |
| [MSC3575](https://github.com/matrix-org/matrix-spec-proposals/pull/3575) | Sliding Sync (v1) | N/A | Implemented (legacy, superseded by MSC4186) |

## Messaging

| MSC | Title | Spec | Status |
| --- | --- | --- | --- |
| [MSC2675](https://github.com/matrix-org/matrix-spec-proposals/pull/2675) | Serverside aggregations of message relationships | 1.3 | Implemented |
| [MSC2676](https://github.com/matrix-org/matrix-spec-proposals/pull/2676) | Message editing | 1.4 | Implemented |
| [MSC2677](https://github.com/matrix-org/matrix-spec-proposals/pull/2677) | Reactions | 1.7 | Implemented |
| [MSC2836](https://github.com/matrix-org/matrix-spec-proposals/pull/2836) | Threading | N/A | Opt-in (`msc2836`) |
| [MSC2285](https://github.com/matrix-org/matrix-spec-proposals/pull/2285) | Private read receipts | 1.4 | Implemented |

## Rooms & Spaces

| MSC | Title | Spec | Status |
| --- | --- | --- | --- |
| [MSC1772](https://github.com/matrix-org/matrix-spec-proposals/pull/1772) | Spaces | 1.2 | Implemented |
| [MSC2946](https://github.com/matrix-org/matrix-spec-proposals/pull/2946) | Spaces summary / room hierarchy | 1.2 | Implemented |
| [MSC3083](https://github.com/matrix-org/matrix-spec-proposals/pull/3083) | Restricted rooms (space-based membership) | 1.2 | Implemented |
| [MSC2403](https://github.com/matrix-org/matrix-spec-proposals/pull/2403) | Knocking | 1.1 | Implemented |
| [MSC3266](https://github.com/matrix-org/matrix-spec-proposals/pull/3266) | Room summary API | 1.15 | Implemented |
| [MSC3765](https://github.com/matrix-org/matrix-spec-proposals/pull/3765) | Rich text in room topics | 1.15 | Not implemented |
| [MSC4267](https://github.com/matrix-org/matrix-spec-proposals/pull/4267) | Forget room on leave ([`m.forget_forced_upon_leave` capability](https://spec.matrix.org/v1.18/client-server-api/#mforget_forced_upon_leave-capability)) | 1.18 | Implemented (opt-in via [`auto_forget_on_leave`](/administration/auto-forget-on-leave/)) |

## Encryption & Security

| MSC | Title | Spec | Status |
| --- | --- | --- | --- |
| [MSC3916](https://github.com/matrix-org/matrix-spec-proposals/pull/3916) | Authenticated media | 1.11 | Implemented |
| [MSC2732](https://github.com/matrix-org/matrix-spec-proposals/pull/2732) | OLM fallback keys | 1.2 | Implemented |
| [MSC3814](https://github.com/matrix-org/matrix-spec-proposals/pull/3814) | Dehydrated devices v2 | N/A | Opt-in (`msc3814`) |
| [MSC4115](https://github.com/matrix-org/matrix-spec-proposals/pull/4115) | Membership metadata on events | 1.11 | Opt-in (`msc4115`) |

## Federation

| MSC | Title | Spec | Status |
| --- | --- | --- | --- |
| [MSC3706](https://github.com/matrix-org/matrix-spec-proposals/pull/3706) | Partial state in `/send_join` (faster joins) | 1.6 | Implemented |
| [MSC2444](https://github.com/matrix-org/matrix-spec-proposals/pull/2444) | Peeking over federation | N/A | Opt-in (`msc2444`) |
| [MSC2753](https://github.com/matrix-org/matrix-spec-proposals/pull/2753) | Peeking via `/sync` | N/A | Opt-in (`msc2753`) |

## Authentication

| MSC | Title | Spec | Status |
| --- | --- | --- | --- |
| [MSC2918](https://github.com/matrix-org/matrix-spec-proposals/pull/2918) | Refresh tokens | 1.3 | Implemented |
| [MSC3861](https://github.com/matrix-org/matrix-spec-proposals/pull/3861) | Next-gen auth (OIDC) | 1.15 | Opt-in (`msc3861`) |

## VoIP

| MSC | Title | Spec | Status |
| --- | --- | --- | --- |
| [MSC4143](https://github.com/matrix-org/matrix-spec-proposals/pull/4143) | MatrixRTC transport discovery | N/A | Implemented |

## Opt-in MSCs

The following MSCs are not enabled by default and must be activated in the `mscs` section of the config file:

```yaml
mscs:
  mscs:
    - msc2836
    - msc2444
    - msc2753
    - msc3814
    - msc3861
    - msc4115
```

### MSC3814: Dehydrated Devices v2

MSC3814 allows clients to store a "dehydrated" device on the server so that encrypted to-device messages (e.g. key shares) can be delivered while the user is offline.
When the user signs in again, the client rehydrates the device and retrieves messages it missed.

**What changes when MSC3814 is enabled:**

- `PUT /_matrix/client/unstable/org.matrix.msc3814.v1/dehydrated_device` — store a dehydrated device with its keys.
- `GET /_matrix/client/unstable/org.matrix.msc3814.v1/dehydrated_device` — retrieve the current dehydrated device metadata.
- `DELETE /_matrix/client/unstable/org.matrix.msc3814.v1/dehydrated_device` — remove the dehydrated device.
- `POST /_matrix/client/unstable/org.matrix.msc3814.v1/dehydrated_device/{deviceID}/events` — retrieve to-device events addressed to the dehydrated device.

**Configuration:**

```yaml
mscs:
  mscs:
    - msc3814
```

No additional configuration is needed beyond enabling the MSC.

### MSC3861: OIDC Delegated Authentication

MSC3861 delegates authentication to an external OpenID Connect (OIDC) provider such as [Matrix Authentication Service (MAS)](https://github.com/element-hq/matrix-authentication-service).
When enabled, Zendrite validates access tokens via OAuth 2.0 token introspection instead of managing passwords directly.

**What changes when MSC3861 is enabled:**

- Password-based registration and login are disabled.
- `GET /login` returns the legacy `m.login.sso` and `m.login.token` flows, so older Element Web/Desktop clients can sign in via the OIDC provider without native OIDC support. The SSO flow is marked `oauth_aware_preferred`, and `/login/sso/redirect` honours the `action=login|register` parameter (`register` maps to `prompt=create` at the provider).
- The SSO flow is bound to the browser that started it: `/login/sso/redirect` issues an HttpOnly, `SameSite=Lax` cookie that the callback must present, so a flow started in one browser cannot be completed in another.
- `redirectUrl` is validated default-deny.
  With `sso_redirect_allowlist` empty, only targets on the homeserver's own origin (from `public_base_url`, or the incoming request's scheme and host when it is not set) are accepted; everything else is rejected with `M_INVALID_PARAM`.
  The browser-binding cookie is not sufficient on its own here: the victim's own browser runs the whole flow and therefore holds the cookie, so an unrestricted `redirectUrl` would let a single crafted link deliver the login token, and with it the provider-issued access and refresh tokens, to an attacker.
- `POST /login` accepts only `m.login.token`, redeeming a login token issued by the SSO callback; redemption returns the provider-issued OIDC access token, refresh token and expiry, plus the device from its scope (a homeserver-local token would never introspect). Other login types return `403 M_FORBIDDEN`.
- `POST /refresh` bridges to the provider's refresh token grant, so legacy clients can renew expiring OIDC access tokens. Expired tokens report `soft_logout: true`, including on the userinfo fallback path, so clients attempt a refresh instead of destroying the session.
- `POST /register`, `/account/password`, `/account/deactivate`, `/logout`, `/logout/all`, `/delete_devices`, and device modification endpoints return `403 M_FORBIDDEN`.
- `/.well-known/matrix/client` includes an `m.authentication` section with the OIDC issuer.
- `GET /_matrix/client/v1/auth_metadata` advertises the OAuth 2.0 authorization server metadata, including the account management URL/actions, device authorization endpoint, and supported prompt values discovered from the provider.
- `GET /capabilities` reports `m.change_password` and `m.3pid_changes` as disabled.
- `/keys/device_signing/upload` uses the `m.oauth` user-interactive auth type, deep-linking to the account management UI to confirm cross-signing resets.
- Device IDs are taken from the stable `urn:matrix:client:device:<id>` scope token (the `urn:matrix:device:` and unstable MSC2967 prefixes are still accepted).
- Expired access tokens return `401 M_UNKNOWN_TOKEN` with `soft_logout: true` so clients refresh at the provider instead of logging out.
- New users are auto-provisioned on first token introspection.

**Configuration:**

```yaml
mscs:
  mscs:
    - msc3861
  msc3861:
    issuer: "https://auth.example.com/"
    client_id: "0000000000000000000ZENDRITE"
    client_secret: "secret"
    client_auth_method: "client_secret_basic"  # or "client_secret_post"
    admin_token: ""                            # optional: static token for admin API access
    account_management_url: ""                 # optional: URL for account management UI
    introspection_endpoint: ""                 # optional: defaults to {issuer}/oauth2/introspect
    authorization_endpoint: ""                 # optional: defaults to {issuer}/oauth2/auth
    token_endpoint: ""                         # optional: defaults to {issuer}/oauth2/token
    registration_endpoint: ""                  # optional: defaults to {issuer}/oauth2/clients/register
    revocation_endpoint: ""                    # optional: defaults to {issuer}/oauth2/revoke
    device_authorization_endpoint: ""          # optional: defaults to {issuer}/oauth2/device/auth
    jwks_uri: ""                               # optional: defaults to {issuer}/oauth2/keys
    account_management_actions: []             # optional: e.g. ["org.matrix.profile", "org.matrix.devices_list"]
    sso_callback_url: ""                       # optional: overrides the SSO callback URL
    public_base_url: ""                        # optional: e.g. "https://matrix.example.com"; used to build client-facing URLs
                                               # (including the SSO callback) instead of trusting request Host headers
    sso_redirect_allowlist: []                 # optional: redirectUrl targets allowed for the legacy SSO flow;
                                               # when empty, only the homeserver's own origin is accepted
```

| Field | Required | Description |
| --- | --- | --- |
| `issuer` | Yes | The OIDC provider URL (e.g. your MAS instance). |
| `client_id` | Yes | OAuth 2.0 client ID registered with the OIDC provider for introspection. |
| `client_secret` | Yes | OAuth 2.0 client secret for introspection. |
| `client_auth_method` | No | Authentication method: `client_secret_basic` (default) or `client_secret_post`. |
| `admin_token` | No | A static bearer token that grants admin access, bypassing OIDC introspection. Useful for service-to-service calls. |
| `account_management_url` | No | URL where users can manage their account (shown in well-known response and advertised as `account_management_uri` in the auth metadata). |
| `account_management_actions` | No | Account management actions supported by the account management URL, advertised as `account_management_actions_supported`. |
| `device_authorization_endpoint` | No | Override the OAuth 2.0 device authorization endpoint (RFC 8628). Defaults to the discovered value, then `{issuer}/oauth2/device/auth`. |
| `introspection_endpoint` | No | Override the introspection endpoint URL. Defaults to `{issuer}/oauth2/introspect`. |
| `public_base_url` | No | The public base URL of the homeserver (e.g. `https://matrix.example.com`). Used to build URLs advertised to clients, including the SSO callback URL when `sso_callback_url` is not set. |
| `sso_redirect_allowlist` | No | `redirectUrl` targets allowed for the legacy SSO flow (e.g. `["https://app.example.com/", "element://"]`). `http(s)` entries are matched on scheme, host and path prefix, so `https://app.example.com` does not match `https://app.example.com.evil.com/`; entries with any other scheme (such as the `element://` desktop deep link) are matched as plain string prefixes. When empty, only redirect targets on the homeserver's own origin are accepted. |

**Known limitations:**

- The legacy SSO compat layer keeps its in-flight state (SSO sessions, the login tokens issued by the callback, and pending cross-signing reset confirmations) in process memory.
  The `/login/sso/redirect`, `/login/sso/callback` and `POST /login` requests of one login must therefore all be handled by the same Zendrite process.
  Running several instances behind a load balancer breaks the flow unless the balancer pins a client's requests to one instance.
  Clients with native OIDC support are unaffected, since they never use the compat layer.
