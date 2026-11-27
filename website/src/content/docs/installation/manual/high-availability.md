---
title: HA storage and recovery
description: Storage prerequisites, recovery readiness, and upgrades for active-passive candidates.
---

HA is opt-in and experimental.
This page lists the storage and recovery prerequisites for an HA deployment.
[High availability](/administration/high-availability/) describes how HA behaves and how to operate it.

Kubernetes Lease ownership and PostgreSQL advisory locks don't fence a suspended former owner.
Zendrite enforces ownership at the database, broker, media, dispatch, and admission boundaries, as [High availability](/administration/high-availability/) describes.
The [HA qualification](/development/ha-qualification/) tests check the failure envelope.
The [qualification report](/development/ha-qualification-report/) records measured recovery times and the remaining limitations.
The prerequisites on this page add to that qualification.

## Supported storage

- Configure one global PostgreSQL database.
  Leave component connection strings empty or identical to the global connection string.
  HA mode doesn't support separate component databases or SQLite.
- Enable only the experimental MSCs qualified for HA, listed in [MSCs in HA mode](#mscs-in-ha-mode).
  Startup refuses any other MSC, including `msc3814`.
- Configure external JetStream addresses, a nonempty homeserver-specific topic prefix, and `global.jetstream.replicas: 3` or `5`.
  HA uses file storage for every stream, including the typing and presence streams.
  At startup, Zendrite waits up to 30 seconds per stream for every configured replica to become current and online.
  It doesn't require a zero-lag snapshot while publishers are active.
- Use S3 media with the same endpoint, region, bucket, and prefix on every candidate, or explicitly set `media_api.storage.local.shared: true`.
  See [Shared filesystem media](#shared-filesystem-media).

### MSCs in HA mode

These MSCs are qualified for HA:

| MSC | Behavior in HA mode |
| --- | --- |
| `msc1763` | The retention configuration endpoint only reports `global.retention`, so it keeps no state of its own. |
| `msc2444` | Qualified for HA. |
| `msc2753` | Qualified for HA. |
| `msc2836` | Leave `mscs.database.connection_string` empty or identical to the global connection string, so Zendrite fences the relationship index like every other table. |
| `msc3391` | Deletions are user API writes to the global database, so it keeps no state of its own. |
| `msc3664` | Zendrite adds the reply rule to the stored push rules when it reads them, and reads related events from the roomserver, so it keeps no state of its own. |
| `msc3848` | Each refused request reads the roomserver's current state, so it keeps no state of its own. |
| `msc3861` | See [Delegated authentication with msc3861](#delegated-authentication-with-msc3861). |
| `msc3983` | Key claims ask the application service on each request and keep nothing of their own. |
| `msc3984` | Key queries ask the application service on each request and keep nothing of their own. |
| `msc4194` | See [Redactions with msc4194](#redactions-with-msc4194). |
| `msc4263` | Each key query only reads the roomserver memberships, so it keeps no state of its own. |
| `msc4293` | The redactions that a kick or ban asks for are roomserver writes to the global database, made by the owner that processes the kick or ban. |
| `msc4370` | Each request reads the roomserver's latest events, so it keeps no state of its own. |
| `msc4512` | Zendrite forwards every request under a claimed prefix to the application service and sends every `fed_proxy` request over federation, so it keeps no state of its own. |

#### Delegated authentication with msc3861

With `msc3861` (delegated authentication, for example through MAS), Zendrite stores the accounts, devices, external ID mappings, and login tokens it provisions in the global database, like every other write.
Zendrite loses what it keeps in process memory on takeover, and the client retries.
See [Delegated authentication](/administration/oidc/#high-availability).

#### Redactions with msc4194

With `msc4194`, every redaction is a roomserver write to the global database.
A request cut short by a takeover keeps the redactions it made, and the client's next request continues the work.
See [MSC4194](/mscs/#msc4194-redacting-a-users-events).

### Shared filesystem media

`media_api.storage.local.shared: true` asserts that every candidate mounts the same durable POSIX filesystem at the configured path.
The filesystem must provide:

- Synchronous client writes and synchronous server-side persistence.
- Atomic rename.
- Coherent reads across clients.

A synchronously mounted NFS export backed by redundant stable storage is one example.
These don't satisfy the contract: an ordinary local volume, an asynchronously written network mount, a per-candidate PVC, or identical path strings alone.
Zendrite can't verify filesystem mount or storage controller durability settings.

## Storage guarantees

### PostgreSQL

PostgreSQL must use synchronous commit and synchronously replicated storage.
Its failover policy must promote only a replica that contains acknowledged transactions.

Set `connect_timeout` in the connection string, for example `postgres://zendrite@db-primary/zendrite?connect_timeout=5`.
When the primary pod is deleted, connections to its address get no reply until the operator repoints the service.
Without a timeout, each new connection attempt blocks its request for as long as the failover takes.
The timeout doesn't shorten the failover itself.
It makes those requests fail and retry instead of hanging.

### JetStream

JetStream nodes must use independent persistent volumes in separate failure domains.
Set `sync_interval: always` for the documented acknowledged-write guarantee across operating system failures.
Replication alone doesn't imply immediate fsync (see the [NATS `sync_interval` reference](https://docs.nats.io/reference/config/jetstream/sync_interval)).

Set `max_payload: 8MB` on every JetStream node to match the embedded server.
With the 1 MiB NATS default, a room event whose state delta exceeds the limit fails to publish.
A join to a room with tens of thousands of members is one example.
The roomserver stores that event but doesn't apply it to the room's current state and doesn't retry the publication.

### S3

S3-compatible services must preserve acknowledged writes across their supported failover and provide read-after-write consistency.

These are deployment guarantees.
Zendrite can't establish them through a stream API.

### What the guarantee covers

The guarantee covers committed database writes and acknowledged JetStream publications within the configured retention limits.
It doesn't promise an atomic transaction across PostgreSQL, JetStream, media, and remote HTTP delivery.

- The room input stream expires messages after 24 hours, typing after 60 seconds, and presence after five minutes.
- Interest retention removes messages after all interested consumers acknowledge them.
  Operator-supplied size and count limits still apply.
- Federation, appservice, and push retries can produce duplicate remote deliveries.

## Existing streams and consumers

Stream updates use the fully prefixed name and preserve operator settings such as size limits.
Zendrite applies explicit replication changes in place, then verifies them against the broker state.
In standalone mode, an omitted or zero `replicas` value preserves existing replica counts and defaults newly created streams to one replica.
HA rejects storage, retention, age, or subject changes rather than risk queued data.

A failed stream update never deletes and recreates the stream, whether HA is enabled or not.
Standalone mode keeps its explicit memory storage option and degraded memory fallback for new streams.
HA never uses that fallback.
Memory-mode startup never changes the package stream templates.

Before you enable HA on an existing installation, migrate its memory-based typing and presence streams and its legacy push and ack-all consumers in an offline maintenance window.
Drain or back up their data and durable acknowledgement positions before you change them.

HA rejects existing durable consumers with incompatible storage, replication, or acknowledgement settings.
It blocks the application's stream and consumer deletion paths.
HA allows the roomserver's headers-only ephemeral push consumer, because it only wakes durable room workers and has no recoverable state.

Consumer validation uses paginated listing with error reporting.
On failure, it cancels further listing requests and drains the consumer channel before it returns.
Don't delete occupied streams or consumers to make validation pass.

## Candidate compatibility and upgrades

Each candidate computes a deployment identity from these inputs:

- The public signing identities.
- The effective PostgreSQL target and session parameters.
- The broker endpoints, domain, prefix, and replication.
- The media namespace.
- The search settings.
- The set of enabled MSCs.
- With `msc3861`, the issuer, the client ID, and whether `admin_token` is set, because they decide which account Zendrite maps an OIDC subject to.
  The client secret and the admin token aren't included.

Every candidate must present the same identity, so enabling or disabling an MSC is an offline upgrade.
The executable isn't part of the identity.
The builds' [upgrade contract](/administration/high-availability/#upgrade-contract) decides which builds may take over from each other.

Disabling `msc2836` leaves its `MSC2836RoomServerConsumerPull` durable, named with the topic prefix, on the roomserver output stream.
HA won't delete it.
Remove it in the same maintenance window, or it retains every roomserver output message.

The stored record excludes signing private keys, database passwords, NATS credentials, and S3 credentials.
Error messages never print the fingerprint inputs.
Rotating a password or token within the same database user, NATS account, and S3 namespace doesn't require resetting compatibility.
NATS credentials on every candidate must resolve to the same account.
Changing an account is an offline namespace migration.

Use the same homeserver configuration on every candidate.
Different builds, including local builds, may share a deployment only where their upgrade contracts admit each other.

### Ownership and compatibility records

Zendrite stores ownership authority in `zendrite_ha_ownership`, separately from the compatibility records.
The Lease annotation `zendrite.codefloe.com/compatibility` pins the election scope before Zendrite grants ownership.
An existing Lease with a missing or mismatched annotation fails closed, including a manually precreated Lease.
The first candidate creates the annotated Lease.

Don't replace an existing Lease to bypass a mismatch.
Zendrite keeps owner history in `zendrite_ha_ownership_history`, independently of the Lease.

After fenced ownership, Zendrite opens a dedicated PostgreSQL connection and:

1. Takes an advisory migration lock.
2. Checks the deployment identity and upgrade contract against `zendrite_ha_compatibility`.
3. Records its own level and the raised floor there before component constructors run.

Standbys read this record but never write it.
The connection keeps the lock through initialization, recovery, and listener binding.
It also has the activation marker, so a successor's sweep terminates it like any other application backend.

If application initialization fails, the compatibility record stays pinned, readiness stops, and Zendrite needs a fresh process.
Migration transactions keep their existing rollback behavior.
Only a build that the record admits retries a partly completed sequence, under safe ownership.
Incompatible schema errors during initialization prevent readiness.

### Migrations during a rolling upgrade

Migrations run only in an activation, after Zendrite has fenced the previous owner and under the migration lock.
The old and new builds therefore never write the schema at the same time, and a standby never changes storage that the running owner uses.

A migration delays the successor's readiness, and a planned handover holds requests for that long.
Keep migrations that run during a rolling upgrade short, and move long data backfills to background work that tolerates restarts.

Within a protocol, a level never raises `upgrade_from` or `rollback_to`, so every migration must leave the schema usable by every earlier level of that protocol.
Split a change that older levels can't work with over several levels:

1. Add the new structure, and read and write both.
2. Stop using the old structure.
3. Keep the old structure rather than dropping or renaming it.

A change that older levels can never work with starts a new protocol in a major release, which needs the offline upgrade below.

`setup/ha/testdata/surface.txt` records every level's schema, migrations, and streams.
CI refuses a change to them that doesn't declare a new level.

### Offline upgrades

You need an offline upgrade when the upgrade contract doesn't admit the new build, or when the deployment identity changes:

1. Stop and fence every candidate.
2. Back up the database, broker, and media.
3. Verify the intended schema migration with the new binary.
4. Deploy the same artifact everywhere.
5. Only after you verify that no former activation can return, reset both compatibility records by deleting the Lease and the `zendrite_ha_compatibility` row.
6. Bootstrap the new configuration.

After a failed or ambiguous migration, keep candidates stopped until you repair the schema or restore the coordinated backup.
Resetting the records isn't a schema migration and doesn't make an old binary safe to run against a newer schema.
Zendrite doesn't support automatic schema downgrade.

## Promotion recovery and search

A fresh process initializes caches and synchronously restores sync stream positions and notifier membership from PostgreSQL.
In HA mode, federation destination listing must succeed.
Recovered destination workers keep the existing staggered retry schedule, so readiness doesn't wait for remote homeservers to become available.
Durable consumers resume from their persisted acknowledgement positions.

Before Zendrite opens public listeners, it waits for the sync consumers to apply their captured stream backlog, including messages already delivered but not acknowledged.
The presence consumer takes part only when inbound or outbound presence is enabled.
Cancellation, broker errors, or a 120-second sync recovery timeout prevent readiness.

Typing state, login and SSO intermediate state, and sliding sync sessions stay transient process state.
After promotion, clients may need to retry or start a fresh session.
With `msc3861`, that state also includes the token introspection cache, legacy SSO compatibility sessions and the provider tokens behind their login tokens, and cross-signing reset confirmations from the provider.
Zendrite records client transaction IDs in the database, so retrying a message after promotion doesn't store it twice.

### Search index rebuild

When search is enabled, set `sync_api.search.rebuild_on_startup: true`.
Every activation then builds a fresh Bleve index from PostgreSQL in the background, so readiness and the public listeners don't wait for it.
Until the rebuild finishes, `/search` answers HTTP 503 with `Retry-After` instead of results that would silently miss history.

Sync consumers index new events during the rebuild.
Zendrite replays the deletions they make for redactions and edits once the rebuild has written its last document.
A batch that the rebuild read before a redaction therefore can't restore the redacted text.
A database read error or index write error is logged, and the rebuild restarts from the beginning.
Ownership cancellation stops the rebuild.

The rebuild advances by the maximum stream position in each batch, independent of map iteration order.
It starts from an empty index, so deleted or redacted documents can't survive from a candidate's older local index.

`sync_api.search.index_path` is a candidate-local workspace for disposable recovery indexes, separate from shared media.
It needs no shared index or persistent volume and must not point to a Bleve index that another process has open.
Normal shutdown removes the activation's temporary index after closing it.
A killed process can leave unused `recovery-*` directories, which you clean up offline.
Provision space for a complete rebuilt index and possible crash leftovers.

Rebuild time grows with the searchable history and delays only search.
`HACHAOS_SEARCH_HISTORY` makes the [HA qualification](/development/ha-qualification/) tests seed a history of a chosen size to measure it.

## Retention purge jobs

With `global.retention.enabled`, the [retention purge jobs](/mscs/#message-retention-policies) run only in the activation, and stop at the next batch once ownership is lost.
The roomserver and the sync API each purge their own tables in batches that only delete expired events, so a pass cut short by a takeover leaves consistent storage.
Each activation runs its jobs a minute after startup, so takeovers more frequent than a job's interval don't stop purging.

## Media fencing boundary and qualification

Media originals use content-hash keys, and PostgreSQL metadata controls their publication.
A repeated original upload is idempotent, but thumbnail replacement and media or thumbnail deletion are mutable operations.
Besides its PostgreSQL and broker barriers, the fencing provider must drain or otherwise exclude previously dispatched mutable media operations before it authorizes a successor.
Shared media doesn't provide that barrier itself and doesn't solve local search index persistence.
The fencing release gate blocks deployments without those proofs.

Automated tests cover:

- Configuration rejection.
- Credential-independent compatibility.
- Lease mismatches.
- PostgreSQL migration serialization.
- Search recovery errors, cancellation, and cursors.
- Namespace-safe stream updates.
- Replication upgrades.
- Preservation of acknowledged messages and pending consumer state across a three-node broker leader failure.

They also cover the ownership failure envelope:

- A leader paused beyond expiry and resumed after takeover is refused at the database, broker, media, admission, and dispatch boundaries.
  This includes a connection pool it had already opened and used.
- A leader partitioned from the Kubernetes API learns through the database alone that it was replaced, while its Lease context is still live and it still has data-plane access.
- A sweep that completes a pass without converging blocks promotion and reports how many backends still belong to the retired generation.
- An application that won't stop within the shutdown budget forces process termination.
- A transaction already dispatched completes, while newly admitted dispatch is refused.

The [HA qualification](/development/ha-qualification/) tests cover PostgreSQL failover and complete Matrix client, sync, media, search, and federation recovery, including against the ADR reference workload.
They don't cover infrastructure power-off, correlated storage failures, or S3 and shared filesystem failover.
Test those on the target infrastructure.
