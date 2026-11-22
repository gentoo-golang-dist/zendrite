---
title: Setting up the domain
description: Domain name configuration, TLS certificates, and delegation for Zendrite
---

Every Matrix server needs a server name that uniquely identifies it.
If your server name is `example.com`, your users get usernames in the format `@user:example.com`.

For federation to work, other homeservers on the internet must be able to resolve the server name.
Register the domain and configure the relevant DNS records.

When federating, Matrix servers usually discover each other as follows:

1. If `example.com` has a well-known delegation, connect to the domain and port from the well-known file.
2. If `example.com` has a DNS SRV delegation, connect to the IP address and port from the SRV record.
3. If neither delegation exists, connect to `example.com` on port TCP/8448 using HTTPS.

[The spec](https://spec.matrix.org/v1.3/server-server-api/#resolving-server-names) describes server name resolution in detail.

## TLS certificates

Matrix federation requires valid TLS certificates on the domain.
Get them from a publicly trusted certificate authority (CA).
[Let's Encrypt](https://letsencrypt.org) is a popular CA because its certificates are publicly trusted, free, and automated through the ACME protocol.
Other homeservers typically reject self-signed certificates, so they don't work for federation.

Automate certificate renewal.
The simplest way is to let your reverse proxy do it.
[Caddy](https://caddyserver.com) is a production-grade reverse proxy with automatic TLS, commonly used in front of Zendrite.
Once your domain name points at your server, Caddy obtains and renews TLS certificates automatically by default.
This page doesn't cover [configuring Caddy](https://caddyserver.com/docs/) in detail, but you must reverse proxy all `/_matrix` paths to your Zendrite server.
For example, with Caddy:

```text
reverse_proxy /_matrix/* localhost:8008
```

The reverse proxy can listen on the standard HTTPS port TCP/443 if your domain delegation points to port TCP/443.

## Delegation

Delegation tells other servers the server name and port where your Zendrite installation is reachable.
It also lets you host Zendrite under a different server name than the delegated domain.

For example, if Zendrite is reachable at `matrix.example.com` port 8448, you can delegate from `example.com` to `matrix.example.com`.
Your users then get `@user:example.com` usernames instead of `@user:matrix.example.com`.

You can delegate in one of two ways:

- **Well-known delegation (preferred)**: a web server on the domain you want to use serves a well-known text file over HTTPS, pointing to your server on `matrix.example.com` port 8448.
- **DNS SRV delegation (not recommended)**: see [DNS SRV delegation](#dns-srv-delegation) below.

If a reverse proxy forwards `/_matrix` to Zendrite, your well-known or delegation must refer to the hostname and port that the reverse proxy listens on.

## Well-known delegation

Well-known delegation requires a web server at `example.com` that listens on the standard HTTPS port TCP/443.

If Zendrite listens for HTTPS connections at `matrix.example.com` on port 8448, serve the delegation file at `https://example.com/.well-known/matrix/server` with this JSON document:

```json
{
  "m.server": "matrix.example.com:8448"
}
```

For example, with Caddy:

```text
handle /.well-known/matrix/server {
    header Content-Type application/json
    header Access-Control-Allow-Origin *
    respond `{"m.server": "matrix.example.com:8448"}`
}

handle /.well-known/matrix/client {
    header Content-Type application/json
    header Access-Control-Allow-Origin *
    respond `{"m.homeserver": {"base_url": "https://matrix.example.com:8448"}}`
}
```

Zendrite can also serve `.well-known` itself.
Set the `well_known_server_name` config option to the value you want for `m.server`.
This is useful if Zendrite is exposed on `example.com:443` and you don't want a separate web server only for the `.well-known` file.

```yaml
global:
...
well_known_server_name: "example.com:443"
```

### Support contacts

When you set `global.support`, Zendrite also serves `/.well-known/matrix/support` ([MSC1929](https://spec.matrix.org/v1.10/client-server-api/#getwell-knownmatrixsupport)).
Clients show these contacts and the support page to users who need help with the homeserver.
Each contact needs a `role` and at least one of `matrix_id` and `email_address`.
The `role` is `m.role.admin`, `m.role.security`, or a namespaced identifier.

```yaml
global:
  support:
    contacts:
      - matrix_id: "@admin:example.com"
        email_address: admin@example.com
        role: m.role.admin
      - email_address: security@example.com
        role: m.role.security
    support_page: https://example.com/support
```

Like the other `.well-known` files, this one must be reachable on `example.com`.
A separate web server there must forward `/.well-known/matrix/support` to Zendrite or serve the document itself.
Without `global.support`, the endpoint returns 404.

## DNS SRV delegation

Avoid this method, because SRV records in Matrix behave in a counterintuitive way.
An SRV record changes only the IP address and port that other servers connect to, not the domain name.
The `Host` header and TLS SNI of federation requests stay `example.com`, even if the SRV record points at `matrix.example.com`.

So the server needs valid TLS certificates for `example.com`, not `matrix.example.com`.
A reverse proxy between them must be configured as if it's `example.com`, because it never sees the name `matrix.example.com` in incoming requests.

If `example.com` and `matrix.example.com` point at the same IP address, an SRV record pointing at `matrix.example.com` is useful only to change the port number.

If you understand how SRV records work and still want to use them, the service name is `_matrix` and the protocol is `_tcp`.
