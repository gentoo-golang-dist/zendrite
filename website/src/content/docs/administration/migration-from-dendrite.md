---
title: Migration from Dendrite
description: Upgrade an existing Dendrite installation to Zendrite
---

Zendrite is a fork of [Dendrite](https://github.com/element-hq/dendrite).
Follow these steps to move an existing Dendrite installation to Zendrite.

## Binary name

The binary is now `zendrite` instead of `dendrite`.
Update systemd unit files, scripts, and process managers that use the old name.

## Config file

The default config file is now `zendrite.yaml` instead of `dendrite.yaml`.
If Zendrite can't find `zendrite.yaml`, it falls back to `dendrite.yaml` and logs a warning.
You can also pass the path explicitly:

```sh
zendrite --config dendrite.yaml
```

Rename the file to `zendrite.yaml` to silence the warning.

The sample config file is now `zendrite-sample.yaml`.

## Admin API endpoints

All admin API endpoints moved from the `/_dendrite/` prefix to `/_zendrite/`.
Update scripts, monitoring, and tools that call them.

For example:

- `/_dendrite/admin/evacuateRoom/{roomID}` → `/_zendrite/admin/evacuateRoom/{roomID}`
- `/_dendrite/admin/registrationTokens` → `/_zendrite/admin/registrationTokens`

## JetStream topic prefix

The default JetStream `topic_prefix` is now `Zendrite` instead of `Dendrite`.
With the built-in NATS server (the default), Zendrite ignores NATS data stored under the old prefix.

You have two options:

1. **Fresh start (recommended).**
   Delete the old JetStream data directory and let Zendrite recreate the streams.
   You lose pending events in the old streams, but Zendrite recreates the streams from the database on startup.
2. **Keep the old prefix.**
   Set `topic_prefix: Dendrite` in your config file to keep using the existing streams.

## Docker

The Docker image changed its user and paths:

- Container user: `dendrite` → `zendrite`
- Config volume: `/etc/dendrite` → `/etc/zendrite`
- Binary path: `/usr/bin/dendrite` → `/usr/bin/zendrite`

Update Docker Compose files and orchestration configs that mount volumes or use these paths.

## Environment variables

`DENDRITE_TRACE_HTTP` is now `ZENDRITE_TRACE_HTTP`.
Zendrite still accepts the old name for backwards compatibility.

## Database

Put the database user, password, and database name you used for Dendrite into `zendrite.yaml`.
Zendrite migrates the database schema on first startup.
It renames the internal `dendrite_version` column in the `db_migrations` table to `zendrite_version`, with no manual steps.

## Automatic purging of empty rooms

Zendrite purges a room once no local user has a non-forgotten membership row.
The `room_server.auto_purge_empty_rooms` setting controls this and defaults to `on_all_forgotten`.

On the first restart after the upgrade, Zendrite purges every room where each local user has left and forgotten it, or never had a membership row.

- To review these rooms before the restart, call [`GET /_zendrite/admin/emptyRooms`](/administration/adminapi/#get-_zendriteadminemptyrooms).
- To opt out, set `room_server.auto_purge_empty_rooms: never` (or the legacy `false`) in your config.

See [Purge empty rooms automatically](/administration/auto-purge-empty-rooms/) for details, including the more aggressive `on_empty` mode.
