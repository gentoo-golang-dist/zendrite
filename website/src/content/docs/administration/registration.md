---
title: Enabling registration
description: Configure user registration and CAPTCHA verification
---

Set `registration_disabled: false` in the `client_api` section to let users create their own accounts from their Matrix client.
They then choose a username and password and log in.
The default is `true`, which disables registration.

Zendrite supports three CAPTCHA providers for secondary verification: [reCAPTCHA](https://www.google.com/recaptcha/about/), [hCaptcha](https://www.hcaptcha.com/), and [ALTCHA](https://altcha.org/) (a self-hosted proof-of-work solution).

## reCAPTCHA verification

If you enable registration, configuring reCAPTCHA is **highly recommended**.
It makes it much harder for automated spam systems to register accounts on your homeserver.

Get an API key from the [reCAPTCHA Admin Panel](https://www.google.com/recaptcha/admin).
Then set these fields in the `client_api` section:

```yaml
client_api:
  # ...
  registration_disabled: false
  recaptcha_public_key: "PUBLIC_KEY_HERE"
  recaptcha_private_key: "PRIVATE_KEY_HERE"
  enable_registration_captcha: true
  captcha_bypass_secret: ""
  recaptcha_siteverify_api: "https://www.google.com/recaptcha/api/siteverify"
```

## ALTCHA verification

[ALTCHA](https://altcha.org/) is an open-source, self-hosted CAPTCHA that uses proof-of-work instead of external API calls, so it has no third-party dependencies or privacy concerns.

1. Generate a random HMAC key of at least 32 characters:

   ```bash
   openssl rand -hex 32
   ```

2. Configure the `client_api` section:

   ```yaml
   client_api:
     # ...
     registration_disabled: false
     enable_registration_captcha: true
     captcha_provider: altcha
     altcha_hmac_key: "your-generated-hmac-key"
     altcha_max_number: 50000    # most SHA-256 attempts a client needs (default 50000)
     altcha_expiry: "5m"         # how long a challenge is valid (default 5m)
   ```

With ALTCHA, you don't need `recaptcha_public_key`, `recaptcha_private_key`, or `recaptcha_siteverify_api`.

The server generates a cryptographic challenge, the client solves it by brute-force hashing, and the server verifies the solution locally with HMAC.
Zendrite issues challenges in the ALTCHA v2 protocol.
The bundled fallback page solves them with version 3 of the ALTCHA widget.
A custom client that renders its own widget needs ALTCHA widget 3 or newer.

## Open registration

Zendrite supports open registration, where users create accounts without any verification or secondary authentication.
Open registration is **not recommended**.
It exposes your homeserver to spammers and other abusers who create large numbers of accounts on Matrix homeservers to send spam or abuse into the network.

You can't enable open registration in a single step.
Zendrite logs an error and fails to start if you set `registration_disabled` to `false` without a secondary verification method such as reCAPTCHA.

## Password length

`client_api.password_minimum_length` sets the minimum number of characters in a new password:

```yaml
client_api:
  password_minimum_length: 8
```

The default is 8.
Set it to 0 for no minimum, which matches the Synapse default.
The setting applies to registration, shared secret registration, password changes and resets, and the admin password reset API.
Zendrite rejects a shorter password with `M_WEAK_PASSWORD`.
Passwords can have at most 512 characters, whatever the setting.

## Registration sessions

Registration through `/register` uses user-interactive authentication.
The first request returns a `session`, and the client must send it back with each stage, such as `m.login.dummy` or a CAPTCHA.
Zendrite doesn't complete a stage that omits the session issued for the requested username, or names a session the server didn't issue.
It answers with HTTP 401 and a new session instead.

A client that never received a session can still complete a single-stage flow, such as `m.login.dummy` or `m.login.registration_token`, in one request.
Application service, shared secret, and guest registration don't use sessions.

## Terms of service

`client_api.registration_terms` lists policy documents that new users must accept, such as terms of service and a privacy policy ([MSC1692](https://spec.matrix.org/v1.11/client-server-api/#terms-of-service-at-registration)):

```yaml
client_api:
  registration_terms:
    - id: terms_of_service
      version: "1.2"
      translations:
        en:
          name: Terms of Service
          url: https://example.com/terms-1.2-en.html
        fr:
          name: Conditions d'utilisation
          url: https://example.com/terms-1.2-fr.html
    - id: privacy_policy
      version: "1.2"
      translations:
        en:
          name: Privacy Policy
          url: https://example.com/privacy-1.2-en.html
```

Every registration flow then ends with the `m.login.terms` stage, and clients show each policy with a link.
Clients that don't support the stage open its fallback page at `/_matrix/client/v3/auth/m.login.terms/fallback/web`.

The `id` and `version` follow the spec's opaque identifier grammar (letters, digits, `.`, `_`, `~`, and `-`).
Each URL must use `http` or `https`.
Zendrite doesn't record which version a user accepted.

## Password reset by email

Set `client_api.email_validation_server` to let users who forgot their password reset it with an email address on their account:

```yaml
global:
  trusted_third_party_id_servers:
    - vector.im
client_api:
  email_validation_server: vector.im
```

Zendrite sends no email itself, so this identity server sends the validation email.
Clients call `POST /_matrix/client/v3/account/password/email/requestToken` and `POST /_matrix/client/v3/account/password` without an access token.

- Zendrite trusts only this server to vouch for an address.
  It ignores an `id_server` named by the client, because an identity server the client chose could claim any address and take over the account.
- Requests for an address that no account has fail with `M_THREEPID_NOT_FOUND`, as the spec requires.
  This reveals to the caller whether an address is registered.
- Zendrite rate-limits both endpoints per client address.
- After the reset, Zendrite logs out all the user's devices, unless the request sets `logout_devices` to `false`.
- Phone numbers aren't supported.
- With MSC3861, the authentication service handles password resets instead.

## Call guests

Call guests let people without a Matrix account join calls on your homeserver by link, while normal registration stays closed.
Enable them in the `client_api` section:

```yaml
client_api:
  # ...
  # Required: every open sign-up becomes a call guest.
  registration_disabled: true
  call_guests:
    enabled: true
    # How long a call guest account lives, as a Go duration.
    lifetime: 24h
```

Call guests work with the standalone [Element Call](https://github.com/element-hq/element-call) app.
When someone opens a call link without an account, Element Call registers a throwaway account with a random username and password.
It sets the display name the person typed and joins the call's room.
With call guests enabled, these sign-ups succeed, but only as restricted call guest accounts that expire after a while.

Call guests require `registration_disabled: true`, and Zendrite refuses to start otherwise.

- While call guests are enabled, every account created through `/register` with `m.login.dummy` (or with the CAPTCHA, if configured) is a call guest.
- Call guest sign-up needs no CAPTCHA or registration token, so the [open registration](#open-registration) check doesn't apply to it.
- Accounts created with a registration token, the shared secret, or an application service are normal accounts, as before.
- A virtual host with `allow_registration: true` still creates normal accounts.
- Call guests are unrelated to the guest accounts of `kind=guest` registration, which `guests_disabled` controls.

Zendrite ignores the username a call guest requests and generates the localpart itself.
The localpart is `call-guest-` followed by 12 random lowercase letters and digits, such as `@call-guest-4k2m9x0qa7bz:example.com`.
The `call-guest-` prefix makes call guests easy to recognise, and stops them from taking a username a member might want or one that looks trustworthy.
No other account can use the prefix.
Registration with a token, the shared secret, or open registration rejects a `call-guest-` username, so a member can never look like a call guest.

Application service namespaces are unaffected.
Clients show the display name a call guest sets (Element Call asks for one) as usual.

### What call guests can do

A call guest can do what a call needs and little else:

- Sync, and read the messages, state, and members of the rooms it joined.
- Join a room by ID or alias, but only a room this homeserver is already in, and only where the room's join rules let it in.
  In practice, these are public rooms or rooms a member invited it to.
- Leave and forget rooms.
- Send messages, reactions, redactions, and call membership events, and schedule [delayed events](/administration/matrixrtc/#delayed-events), as the room's power levels allow.
- Change its own room membership, for example its display name in a room.
- Upload, query, and claim end-to-end encryption keys, and send to-device messages.
- Discover the MatrixRTC transports and request an OpenID token, which lk-jwt-service needs to admit it to the call.
- Set its display name.
- Manage its own devices, filters, account data, receipts, read markers, and typing notifications.
- Read its push rules, download media, and deactivate its own account.

### What call guests cannot do

Zendrite answers a call guest with `403 M_FORBIDDEN` on every other authenticated endpoint.
In particular, a call guest can't:

- Create rooms, including direct messages.
- Invite, kick, ban, or unban anyone, or knock on a room.
- Send `m.room.member` events about other users.
- Send the state events that change who can join a room and what the room is, even where the room's power levels would allow it: `m.room.create`, `m.room.power_levels`, `m.room.join_rules`, `m.room.history_visibility`, `m.room.guest_access`, `m.room.encryption`, `m.room.canonical_alias`, `m.room.aliases`, `m.room.third_party_invite`, `m.room.server_acl`, `m.room.tombstone`, `m.space.child`, and `m.space.parent`.
- Join a room over federation, or resolve another server's room alias.
- Search the user directory, upload media, change room aliases or the room directory, upgrade rooms, or report content.
- Change its avatar or other profile fields, its password, or its third-party identifiers.
- Set up key backup or pushers.
- Use the admin APIs.

Zendrite doesn't add call guests to `auto_join_rooms`.

### Expiry

Every ten minutes, Zendrite deactivates call guests whose accounts are older than `lifetime`.
Deactivation works the same as when users deactivate their own account.
The call guest leaves all its rooms, and Zendrite deletes its devices, pushers, and delayed events.
If a call is still running when the account expires, the call ends for that guest.

The sweep also runs while `call_guests.enabled` is `false`, so turning call guests off still ends accounts created earlier.
Under [high availability](/administration/high-availability/), only the active replica runs it.

### Element Call and lk-jwt-service

Set your homeserver as the default server of the standalone Element Call app.
Make sure [MatrixRTC](/administration/matrixrtc/) is set up, including delayed events.
Element Call creates call rooms with the `public_chat` preset, unlisted in the room directory, so a call guest can join them from the call link.

lk-jwt-service checks each participant's OpenID token with their homeserver.
Call guests are local users of your homeserver.
If lk-jwt-service's `LIVEKIT_FULL_ACCESS_HOMESERVERS` lists your homeserver, call guests get full access too.
A call guest can then both join a running call and start one in any public room on your homeserver that it joins.

Call guests don't apply when [authentication is delegated](/administration/oidc/), because the authentication service handles registration then.

Before you roll back to a Zendrite version without call guests, disable them and let the existing call guests expire.
Older versions treat call guest accounts as normal users.
