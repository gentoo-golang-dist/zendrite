---
title: Backups
description: What to back up for a Zendrite homeserver.
---

Include the following persistent data in your backups.
Use the same list when you move Zendrite to a new server.

## Configuration

- **Server signing key**: `matrix_key.pem` by default, set by `global.private_key`.
  This is the most critical item.
  Without it, other servers can no longer verify events your server has already signed.
  If you have rotated keys, also back up every file listed under `global.old_private_keys`.
- **Zendrite configuration file**: `zendrite.yaml` or equivalent.

## Database

Back up your database with a dedicated backup tool for your database type.

## Persistent directories

- **JetStream storage**: the directory set by `global.jetstream.storage_path` (default `./`).
  It contains the NATS JetStream streams that Zendrite components use to exchange messages.
  Without it, a restore may lose some in-flight events.
- **Media store**: the directory set by `media_api.base_path` (default `./media_store`).
  It contains all uploaded media and cached remote media.
- **Search index**: the directory set by `sync_api.search.index_path` (default `./searchindex`).
  A full re-index rebuilds it from the database, so a backup is optional but speeds up a restore.
