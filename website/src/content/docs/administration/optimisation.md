---
title: Optimise your installation
description: Performance tuning and optimization tips for Zendrite.
---

These settings improve the reliability and performance of a running Zendrite installation.

## PostgreSQL connection limit

PostgreSQL accepts only a limited number of connections.
The `max_connections` and `superuser_reserved_connections` settings in `postgresql.conf` usually set this limit.
Once the limit is reached, **PostgreSQL immediately stops accepting new connections** until existing ones close.
This is a common misconfiguration.

For example, with `max_connections` set to `100` and `superuser_reserved_connections` set to `3`, the effective limit is 97 connections.
Keep Zendrite below that limit.
Otherwise database queries fail unexpectedly, which causes problems both in Zendrite and for users.

If other software uses the same PostgreSQL server, its connections reduce the number left for Zendrite.
Check the connection limits that other software configures and subtract them.

Set how many connections Zendrite opens with `max_open_conns` in each `database` block.

**If you use the `global` database pool**, set `max_open_conns` once in the `global` section.

If you raise `max_connections` on your PostgreSQL server, raise `max_open_conns` in your Zendrite configuration to match.
Do this only on servers with enough capacity to run the extra queries at the same time.

## PostgreSQL optimisation

PostgreSQL uses few system resources by default and performs better when it may use more.
The [PGTune](https://pgtune.leopard.in.ua/) tool generates example configurations.
Before you change settings, consider your setup, such as other software on the same machine or PostgreSQL server.

## Reclaim disk space

Zendrite removes data it no longer needs in the background, and the database then reuses that space for new data.
PostgreSQL keeps the files of a table at their size until the table is rewritten, so the database does not shrink on disk by itself.

The state delta compaction is one such cleanup.
Servers in large rooms that ran a version before 3.6.0 stored the whole room state again with every gap fill, which can take several gigabytes in the `syncapi_output_room_events` table.
When the compaction has removed this data, Zendrite logs `State delta compaction: removed redundant state event IDs` with the number of rows and state event IDs.

To return the space to the filesystem after that message, rewrite the table:

```sql
-- PostgreSQL
VACUUM FULL syncapi_output_room_events;
```

```sql
-- SQLite, which rewrites the whole database file
VACUUM;
```

`VACUUM FULL` locks the table for reads and writes while it runs, so syncs wait until it completes.
It needs free disk space for the new copy of the table, which is about the size the table has after the compaction.
Run it at a quiet time.
You don't need to run it at all: without it, the database fills the freed space before it grows again.

## State block compaction

Servers in large rooms that ran a version before 3.11 stored the whole state of such a room again whenever it merged two branches of the room's history.
Zendrite re-encodes these old copies in the background as their differences from an earlier copy, which can take the `roomserver_state_block` table from gigabytes to a fraction of that.

It runs a few minutes after start and then every hour, on the active instance only, until it has gone through every room once.
Each block takes two steps at least ten minutes apart, so that no request still uses the old list of blocks.
Each step is one transaction that leaves every room's state unchanged.
A restart, a lost connection or a handover between the steps only delays it.
Zendrite logs `State compaction: pass finished` after each pass and `State compaction: finished` when it is done, and doesn't run it again after that.

Like the state delta compaction, it frees space that the database reuses but doesn't return to the filesystem.
To return it, rewrite the table at a quiet time with `VACUUM FULL roomserver_state_block;` as described in [Reclaim disk space](#reclaim-disk-space).

## File descriptor limit

Most platforms limit how many file descriptors one process can open.
Every connection Zendrite makes uses a file descriptor, including database connections and requests to remote homeservers.
In large federated rooms, Zendrite talks to many remote servers and can exhaust the low default limits.

Set the file descriptor limit to 65535.
Zendrite logs a warning at startup if the limit is too low:

```text
level=warning msg="IMPORTANT: Process file descriptor limit is currently 1024, it is recommended to raise the limit for Zendrite to at least 65535 to avoid issues"
```

UNIX systems have a hard limit and a soft limit.
Show the soft limit with `ulimit -Sn` and the hard limit with `ulimit -Hn`:

```bash
$ ulimit -Hn
1048576

$ ulimit -Sn
1024
```

Raise the soft limit before you start Zendrite:

```bash
ulimit -Sn 65535
```

Once the limit is high enough, the startup warning no longer appears.

Under a systemd service, add `LimitNOFILE=65535` to the `[Service]` section of the unit file instead.

## DNS caching

Zendrite has a built-in DNS cache that reduces the load on your DNS resolver.
It may also speed up outbound federation.

Enable it in the `global` section of your configuration file:

```yaml
dns_cache:
  enabled: true
  cache_size: 4096
  cache_lifetime: 600s
```

## Time synchronisation

Matrix relies on TLS, which needs a correct system clock.
If the clock drifts, federation may fail partly or completely, and clients may fail to connect to your Zendrite server.

Enable NTP sync to keep the system time correct.
