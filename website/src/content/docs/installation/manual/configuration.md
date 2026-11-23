---
title: Configuring Zendrite
description: YAML configuration options for Zendrite
---

Zendrite reads its configuration from a YAML file.
Copy `zendrite-sample.yaml` from the top level of the Zendrite repository, for example to `zendrite.yaml`, and adapt it to your installation.
At a minimum, fill in the following sections.
Then [start Zendrite](/installation/manual/starting).

## Server name

In the `global` section, set `server_name` to your delegated domain name.
It must match the domain name you chose when [configuring the domain name delegation](/installation/domainname#delegation).

```yaml
global:
  # ...
  server_name: example.com
```

## Server signing keys

In the `global` section, set `private_key` to the path of your [server signing key](/installation/manual/signingkey):

```yaml
global:
  # ...
  private_key: /path/to/matrix_key.pem
```

## JetStream configuration

Zendrite can use its built-in NATS Server or a standalone NATS Server.

### Built-in NATS Server

In the `global` section, under the `jetstream` key, configure no server addresses and set `storage_path` to a persistent folder on the filesystem:

```yaml
global:
  # ...
  jetstream:
    storage_path: /path/to/storage/folder
    topic_prefix: Zendrite
```

### Standalone NATS Server

To use a standalone NATS Server instance, set the `addresses` field to the port your NATS Server listens on:

```yaml
global:
  # ...
  jetstream:
    addresses:
      - localhost:4222
    topic_prefix: Zendrite
```

A standalone NATS Server doesn't need `storage_path`.
For a multi-node NATS cluster, list more than one address in the `addresses` field.

Set `max_payload: 8MB` in the NATS server configuration to match the embedded server.
Room events include their state delta, so joins to large rooms can exceed the 1 MiB NATS default, and those events then fail to publish.
Zendrite logs a warning at startup when the server advertises a smaller limit.

The roomserver keeps a NATS consumer for every room that saw activity in the last day.
Zendrite gives its streams their own consumer limit, so the 1000 consumer default of NATS Server 2.15 and later doesn't apply to them.
A consumer limit on the NATS account still applies: keep it unset, or above the number of rooms active on a busy day.
Zendrite logs a warning at startup when the account limits consumers.

### Authenticating to NATS

If your NATS deployment requires authentication, Zendrite supports the following methods:

- **NATS credentials file**: set `credentials_path` to a `.creds` file.
  Zendrite rereads the file on every reconnect, so you can rotate credentials without restarting Zendrite.

  ```yaml
  global:
    # ...
    jetstream:
      addresses:
        - nats://nats.example.com:4222
      credentials_path: /etc/zendrite/nats.creds
      topic_prefix: Zendrite
  ```

- **Username and password in the URL**: include the credentials directly in the `addresses` entry.

  ```yaml
  global:
    # ...
    jetstream:
      addresses:
        - nats://user:password@nats.example.com:4222
      topic_prefix: Zendrite
  ```

Both methods also work with NATS [auth callout](https://docs.nats.io/running-a-nats-service/configuration/securing_nats/auth_callout), because client-side authentication stays the same.
Zendrite presents the configured credentials on every connect and reconnect, and the auth callout service decides whether to accept them.

If the configured credentials are missing or rejected, Zendrite logs a clear authentication error and exits with a non-zero status instead of hanging.

## Database connection using a global connection pool

To use a single connection pool to a single PostgreSQL database, configure the `database` section within the `global` section:

```yaml
global:
  # ...
  database:
    connection_string: postgres://user:pass@hostname/database?sslmode=disable
    max_open_conns: 90
    max_idle_conns: 5
    conn_max_lifetime: -1
```

See also [Connection strings](/installation/manual/database/#connection-strings).

A per-component `database` section overrides the `global` database configuration.
You can add one under the `app_service_api`, `federation_api`, `key_server`, `media_api`, `mscs`, `relay_api`, `room_server`, `sync_api`, and `user_api` blocks.

## Full-text search

Zendrite indexes messages for full-text search with [Bleve](https://github.com/blevesearch/bleve).
Configure it in the `sync_api` section.
Set `language` to the language most used on the server, so search results match expectations.
`internal/fulltext/bleve.go` lists all supported languages.

```yaml
sync_api:
  # ...
  search:
    enabled: false
    index_path: "./searchindex"
    language: "en"
```

If you enable search later, [reindex existing rooms through the admin API](/administration/adminapi/#get-_zendriteadminfulltextreindex).

## Media storage

By default, Zendrite stores media uploads and generated thumbnails on the local filesystem.
To use an S3-compatible object store instead, change the `storage` block under `media_api`.

### Local filesystem

```yaml
media_api:
  storage:
    type: local
    local:
      base_path: /path/to/media_store
```

The legacy `base_path` option still works for backwards compatibility.
If `storage.local.base_path` is not set, Zendrite falls back to `media_api.base_path`.

### S3-compatible object storage

```yaml
media_api:
  storage:
    type: s3
    s3:
      endpoint: "https://s3.amazonaws.com"
      region: "us-east-1"
      bucket: "zendrite-media"
      access_key_id: ""
      secret_access_key: ""
      session_token: ""
      path_style: false
      prefix: ""
```

- `endpoint` must include the scheme, for example `https://s3.amazonaws.com` or `http://localhost:9000`.
- `path_style` forces path-style addressing (`https://example.com/bucket-name/key`) instead of virtual-hosted style (`https://bucket-name.example.com/key`).
  MinIO and other self-hosted S3-compatible services usually require path-style.
- If you omit `access_key_id` and `secret_access_key`, Zendrite uses the AWS SDK default credential chain, such as environment variables, IAM roles, and shared credential files.
- `prefix` is optional, and Zendrite prepends it to every object key.

## OIDC delegated authentication (MSC3861)

Zendrite can delegate authentication to an external OIDC provider such as [Matrix Authentication Service (MAS)](https://github.com/element-hq/matrix-authentication-service).
When you enable it, Zendrite stops managing passwords and login sessions.
The OIDC provider handles all authentication through token introspection.

See the [MSC3861 documentation](/mscs#msc3861-oidc-delegated-authentication) for full configuration details.

## Other sections

Review the remaining options in the sample file.
If you federate into public rooms, configure `key_perspectives` (like `matrix.org` in the sample).
Your homeserver can then fetch public keys for dead homeservers from another live server, which improves reliability considerably.
