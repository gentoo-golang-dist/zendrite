---
title: FAQ
description: Frequently asked questions about Zendrite
---

Much of this content comes from [element-hq/dendrite](https://github.com/element-hq/dendrite) and may be out of date after the many changes in this fork.
Use it with caution, and report drift in an issue or pull request.

## Why does Zendrite exist?

Zendrite is a Matrix-compatible server that uses fewer resources than [Synapse](https://github.com/element-hq/synapse).
It also gives more flexibility when scaling up or down.
Its code is easy to change, which makes it suitable for experimenting with new Matrix features.
It was forked from Dendrite to add [significant features](/mscs), optimisations, and fixes.

## Is Zendrite stable?

Mostly.
It advertises Matrix spec versions up to v1.19, and it's much more stable than Dendrite used to be.
If you're a confident power user, try Zendrite.

## Is Zendrite feature-complete?

For the Matrix specification, mostly.
Zendrite advertises spec versions up to v1.19, and the [client API conformance audit](/development/client-api-conformance/) records the evidence.
Some proposals (MSCs) that clients use but that aren't in a spec release yet are still missing.
[Issue #558](https://codefloe.com/pat-s/zendrite/issues/558) tracks them, and [MSC support](/mscs) lists what's implemented.

## Is there a migration path from Dendrite to Zendrite?

Yes, see [Migration from Dendrite](/administration/migration-from-dendrite).

## Is there a migration path from Synapse to Zendrite?

There's [a migration script](https://github.com/Saimo/synapse-dendrite-migration), but it's unsupported and may need maintenance.
If you consider it, take backups and test carefully first.

## Can I use Zendrite with an existing Synapse database?

No.
Zendrite's database schema is very different from Synapse's, and the two aren't interchangeable.

## Can I configure which port Zendrite listens on?

Yes, use the `-http-bind-address` command-line flag.

## I've installed Zendrite but federation isn't working

Check the [Federation Tester](https://federationtester.matrix.org).
You need at least:

- A valid domain name
- A valid TLS certificate for that domain name
- Either SRV records or well-known files, see [Well-known delegation](/installation/domainname/#well-known-delegation)

## Whenever I try to connect from Element it says unable to connect to homeserver

Check that your Zendrite instance is running.
If it is, the cause is most likely a misconfigured reverse proxy.

## Does Zendrite work with my favourite client?

It should.
If a client misbehaves with Zendrite, open an issue.

## Is there a public instance of Zendrite I can try out?

No, there is no public Zendrite instance.

## Does Zendrite support Space Summaries?

Yes.

## Does Zendrite support Threads?

Yes, threads (`m.thread` relations) work without any configuration.

The older threading proposal [MSC2836](https://github.com/matrix-org/matrix-spec-proposals/pull/2836) is a different, experimental feature that clients don't use for threads.
You can opt into it in the `mscs` configuration:

```yaml
mscs:
  mscs:
    - msc2836
```

See [MSC support](/mscs) for the other proposals Zendrite implements.
Treat opt-in MSCs as experimental, because enabling them can cause significant usability problems.
For how MSCs are ratified and their current status, see the [Matrix specification documentation](https://spec.matrix.org/proposals/).

## Does Zendrite support push notifications?

Yes.
Configure them as usual in your Matrix client.

## Does Zendrite support application services/bridges?

Partly.
Zendrite has some application service support, but it isn't well tested.
If you try it and run into problems, open an issue.

Bridges known to work:

- [Telegram](https://docs.mau.fi/bridges/python/telegram/index.html)
- [WhatsApp](https://docs.mau.fi/bridges/go/whatsapp/index.html)
- [Signal](https://docs.mau.fi/bridges/python/signal/index.html)
- [probably all other mautrix bridges](https://docs.mau.fi/bridges/)

Add each bridge's registration file to the `app_service_api` section of the Zendrite config file.

## Is it possible to prevent communication with the outside world?

Yes, disable federation.
Set `disable_federation` to `true` in the `global` section of the Zendrite config file.

## How can I migrate a room to change its internal ID?

Upgrade the room.
In Element, run `/upgraderoom <version>`.

## How do I reset somebody's password on my server?

Use the [`resetPassword`](/administration/adminapi#post-_zendriteadminresetpassworduserid) admin endpoint.

## Should I use PostgreSQL or SQLite for my databases?

Use PostgreSQL wherever possible, especially if your homeserver serves more than a couple of users.

## What data needs to be kept if transferring/backing up Zendrite?

Keep these files:

- `matrix_key.pem`
- `zendrite.yaml`
- the PostgreSQL or SQLite database
- the JetStream directory
- the media store
- the search index (Zendrite can regenerate it)

This list may be out of date.
There are no official instructions for migrations like this.

## How can I prepare enough storage for media caches?

Try [matrix-media-repo](https://github.com/turt2live/matrix-media-repo).
Zendrite doesn't officially support it or any other dedicated media storage solution.

## Zendrite is using a lot of CPU

Expect some CPU spikes, especially when you join or take part in large rooms.
Constant high CPU usage isn't expected.
If you see it, open an issue and describe what you were doing when the CPU usage rose.
A [CPU profile](/development/profiling) helps a lot, because it shows where the CPU time goes.

## Zendrite is using a lot of RAM

As with CPU usage, expect some memory spikes while Zendrite does heavy work.
If it uses more RAM than you expect for a long time, that's probably a problem.
A [memory profile](/development/profiling) helps a lot, because it shows where the memory goes.

## Do I need to generate the self-signed certificate if I'm going to use a reverse proxy?

No.
If your reverse proxy terminates TLS with a proper certificate from a provider such as Let's Encrypt, you can use HTTP between the proxy and the Zendrite process.

## Zendrite is running out of PostgreSQL database connections

Raise the connection limit (`max_connections`) of your PostgreSQL server, lower the connection limits in your Zendrite configuration, or both.
With the `global.database` pool, all components share one pool, so its `max_open_conns` is the limit for the whole server.
Each component with its own `database` block and a different connection string opens a separate pool, and its limit adds to the global one.

## VOIP and Video Calls don't appear to work on Zendrite

Your server's STUN/TURN configuration is the likely cause.
If you believe your configuration is correct, see [Troubleshooting](/administration/troubleshooting).

## What is being reported when enabling phone-home statistics?

Phone-home statistics contain your server's domain name, some information about your deployment's configuration, and aggregated information about its active users.
Zendrite sends them only to the endpoint URL configured in your Zendrite configuration file.
Here is an example of the data it sends:

```json
{
  "cpu_average": 0,
  "daily_active_e2ee_rooms": 0,
  "daily_active_rooms": 0,
  "daily_active_users": 97,
  "daily_e2ee_messages": 0,
  "daily_messages": 0,
  "daily_sent_e2ee_messages": 0,
  "daily_sent_messages": 0,
  "daily_user_type_bridged": 2,
  "daily_user_type_native": 97,
  "database_engine": "Postgres",
  "database_server_version": "17.6",
  "federation_disabled": false,
  "go_arch": "amd64",
  "go_os": "linux",
  "go_version": "go1.26.0",
  "homeserver": "my.domain.com",
  "log_level": "trace",
  "memory_rss": 93452,
  "monolith": true,
  "monthly_active_users": 97,
  "nats_embedded": true,
  "nats_in_memory": true,
  "num_cpu": 8,
  "num_go_routine": 203,
  "r30v2_users_all": 0,
  "r30v2_users_android": 0,
  "r30v2_users_electron": 0,
  "r30v2_users_ios": 0,
  "r30v2_users_web": 0,
  "timestamp": 1651741851,
  "total_nonbridged_users": 97,
  "total_room_count": 0,
  "total_users": 99,
  "uptime_seconds": 30,
  "version": "3.6.0"
}
```
