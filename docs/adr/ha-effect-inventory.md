# Homeserver application-effect inventory

Related: #346 and #347.
All effects below belong to the safely authorized activation, including work started while recovering.
No entry is permitted during standby or election-only acquisition.
ADR 0003 defines node fencing and the database/broker drain barriers; local cancellation alone is not fencing.

| Entry point | Effects and lifetime | Shutdown and ownership requirement |
| --- | --- | --- |
| `cmd/zendrite/homeserver.go` | DNS/client creation, connection manager, caches, all component constructors, optional MSC hooks, HTTP binding | Run once after authorization; join partial startup; never reuse global state after demotion |
| `roomserver.NewInternalAPI`, `SetFederationAPI` | Schema setup, ACL/cache loading, room input consumers, partial-state tracking, initial empty-room sweep | Cancel and join room workers and purge jobs before database closure; fence migrations and event writes |
| `roomserver/internal/input` | Per-room pull consumers, event persistence, state resolution, NATS output and acknowledgements | Inherit activation context; track worker/callback execution; database and broker barriers cover old operations |
| `roomserver/internal/perform` | Background auto-purge and request-driven administration | Jobs may outlive a request but not the activation; complete purge tracking on refused launch |
| `federationapi.NewInternalAPI` | Federation database migrations, queue recovery, stream consumers, partial-state worker, expired-EDU maintenance | Join senders, consumers, and maintenance before closing pools/NATS; external requests already dispatched may complete |
| `federationapi/queue` and `statistics` | Destination sender goroutines, backoff callbacks, database queue updates, remote federation HTTP | Stop new work on cancellation; recover durable transactions; retain protocol retry identity |
| `userapi.NewInternalAPI` | Account/key schema, stale-device cleanup, device-list refresh workers, event/receipt consumers, notification maintenance | Track updater/retry loops; cancel DB and federation operations; preserve data until users stop |
| `userapi/storage/*/stats_table.go` | Periodic daily-visit writes previously scheduled by constructors with detached timers | Schedule against activation context after table preparation; no independent immortal timer |
| `userapi/util` and `consumers` | Push requests, message statistics, phone-home reporting | Use application lifetime for asynchronous sends; bound HTTP operations; join before storage closes |
| `appservice.NewInternalAPI` | Bot account/device provisioning, appservice event consumers and delivery | Provision with activation context; join consumers; permit only documented remote retries |
| `setup.Monolith.AddAllPublicRoutes` | Initializes client, federation, media, sync, optional relay routes; creates transaction cache | Treat registration as application initialization, not standby routing setup |
| `syncapi.AddPublicRoutes` | Sync schema, local Bleve index, notifier/stream position recovery, metadata workers, event consumers | Recovery uses activation context; join users before index closure; full HA index readiness belongs to #350 |
| `syncapi/sync` | Sync waiters, last-seen updates, presence cleanup/delayed publication, sliding-sync session state | Cancel requests and maintenance; rebuild process-local state on next activation |
| `clientapi/routing` | Server-notice provisioning, registration nonce cache, rate-limit/transaction cleanup, SSO sessions, asynchronous admin actions | Owned by activation; request admission closes on cancellation; background actions retain application context |
| `mediaapi` and `filestore` | Backend initialization, local/S3 uploads, thumbnails, downloads, metadata publication | All handlers belong to activation; shared-media immutability and late-operation barriers require #350 validation |
| `setup/jetstream` | Embedded server or external connection, stream create/update/delete, consumer setup, callbacks | Prepare only during activation; join consumers before closing connection/server; HA disables embedded storage later |
| `internal/sqlutil` | Connection pools, schemas, migrations, serialized SQLite writer | Register dependency cleanup after application users; existing uncancellable constructor calls are contained by forced process exit |
| `internal/caching`, `fulltext`, transaction/auth caches | Internal library workers, expiry callbacks, metrics and process-global registries | Close owned caches/indexes after users; process restart contains singleton state; no in-process reacquisition |
| `setup/mscs` | Optional schemas and global event hooks, including experimental relation writes | Activation-only installation; inherited hooks must never be reused after stop; strict HA support requires validating enabled MSCs in #350 |
| `setup/base` | Client/federation/admin/media listeners, request contexts, listener shutdown | Bind before reporting successful startup; track requests; close admission on cancellation; wait before releasing dependencies |
| `cmd/zendrite/main.go` | Logging, profiling, tracing, Sentry, process metrics, signals and Windows service exit | Process-wide observability may run in standby; it must not initialize application state |

## Remaining release gates

The lifecycle refactor is not a distributed ownership implementation.
Issue #348 adds election, #349 implements the external fencing contract, #350 validates durable recovery and storage barriers, #351 adds deployment/probes, and #352 proves the failure envelope.
Legacy schema constructors and optional experimental features still contain operations without context-aware APIs; a blocked operation triggers bounded process termination, never unsafe reuse.
The forced-exit path and infrastructure fence are required even after cooperative cancellation is improved.
