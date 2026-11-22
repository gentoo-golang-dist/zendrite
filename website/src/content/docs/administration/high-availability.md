---
title: High availability
description: Configure active-passive high availability on Kubernetes.
---

High availability (HA) runs one active Zendrite instance while other instances wait as standbys.
If the active instance fails, a standby takes over after it confirms that taking over is safe.
HA is experimental and disabled by default.

Zendrite enforces ownership at every protected boundary: the database, the message broker, media, outbound dispatch, and HTTP admission.
This protects shared resources from stale leaders without cloud provider or hypervisor power-control integration.
The [HA qualification](/development/ha-qualification/) fault tests check the failure envelope.
The [qualification report](/development/ha-qualification-report/) records measured recovery times and the remaining limitations.
Qualify HA on your own infrastructure before you rely on it in production.

## Configuration

Configure HA in the `global.ha` section:

```yaml
global:
  ha:
    enabled: false
    backend: kubernetes
    lease_name: matrix-example-org
    lease_namespace: matrix
    candidate_id: pod-uid
    lease_duration: 30s
    renew_deadline: 20s
    retry_period: 5s
    ownership_timeout: 45s
    status_address: :8009
    handover_grace: 2s
    handover_timeout: 15s
```

Enable HA only once the [prerequisites](#prerequisites) are in place.
All instances of the same homeserver must use the same Lease name and namespace.
Use a separate Lease for each homeserver, and set `candidate_id` to each pod's UID.

The example uses the default timing values.
If you change them, `lease_duration` must be a whole number of seconds and greater than `renew_deadline`.
`renew_deadline` must exceed 1.2 times `retry_period`.
Longer timings allow more time for Kubernetes API requests but can delay failover.

`ownership_timeout` limits how long a candidate may spend proving that the previous owner can no longer act.
It must be at least `lease_duration`, so a successor can outwait an expiring Lease.
A candidate that exhausts this budget stays unready and reports the boundary that blocked it.
It never activates as a fallback.

### Choosing election timings

Election timings trade how fast Zendrite replaces a crashed owner against how long a healthy owner survives a stalled Kubernetes control plane.
Start from one of these profiles:

| Setting | Default | Faster failover |
| --- | --- | --- |
| `lease_duration` | 30s | 20s |
| `renew_deadline` | 20s | 15s |
| `retry_period` | 5s | 2s |
| `ownership_timeout` | 45s | 30s |
| Crash detected within | 35 seconds | 22 seconds |

- **Default:** Use with any control plane, including etcd members spread across data centers and managed control planes whose stall behavior is unknown.
- **Faster failover:** Use with a control plane whose Lease write latency you monitor and have seen stay well under 15 seconds, including during etcd leader elections.

The Helm chart's `ha.leaseDuration`, `ha.renewDeadline`, `ha.retryPeriod`, and `ha.ownershipTimeout` values set these fields, and its defaults match the default profile.
Timings aren't part of the deployment identity, so a normal rolling update can change them.

Follow these rules when you deviate from a profile:

- `renew_deadline` is how long an owner tolerates a control plane that doesn't answer.
  An owner whose renewal doesn't complete within it gives up leadership and exits, even if the control plane recovers a moment later.
  An etcd leader election stalls every Lease write while it lasts, often for several seconds, and longer when etcd members span data centers.
  Set `renew_deadline` at least 5 seconds above the longest Lease write latency you have observed, and never below 10 seconds.
  With `renew_deadline: 6s`, one production deployment lost its owner to a Lease write that took about 8 seconds during an etcd leader election.
  15 seconds would have kept it.
- `lease_duration` is how long a crashed owner goes unnoticed.
  A standby takes over at most `lease_duration` plus `retry_period` after the owner's last renewal.
  Keep it at least 5 seconds above `renew_deadline`, so an owner that gives up has begun stepping down before a successor can acquire the Lease.
  Fencing still refuses anything the owner hasn't stopped.
- `retry_period` spaces out renewals and acquisition attempts.
  Keep it at most a quarter of `renew_deadline`, so Zendrite retries a renewal that fails fast several times before the deadline, for example on a refused connection.
  Shorter periods detect a released Lease sooner but add Lease writes on every candidate.
- `ownership_timeout` must be at least `lease_duration`.
  Leave about 10 seconds more, so fencing a retired owner that is slow to disconnect doesn't exhaust it.

A graceful shutdown releases the Lease, so a standby takes over at its next `retry_period`, whatever the Lease duration.
After takeover, the successor opens its listeners a few seconds after it establishes ownership, and the chart's router adds up to its 500 ms health check interval.
Watch `zendrite_ha_renewal_failures_total`.
Renewal failures without a leadership loss mean the control plane is approaching `renew_deadline`.
Validate actual service recovery with the chosen timings, for example with the [HA qualification](/development/ha-qualification/) tests.

## Probes and status

Every candidate serves liveness, readiness, status, and metrics on `status_address`, separately from the Matrix listeners.
A standby never binds the Matrix listeners.
Without this listener, Kubernetes would see a standby as dead and restart it, and nothing could scrape its metrics until it was promoted.

| Endpoint | Meaning |
| --- | --- |
| `/_zendrite/monitor/up` | Liveness. Succeeds for any running candidate, including a standby. |
| `/_zendrite/monitor/health` | Readiness. Succeeds only for the active homeserver, and answers 404 while it hands over. |
| `/_zendrite/monitor/status` | Phase, leadership, readiness, and the blocking reason as JSON. |
| `/metrics` | Prometheus metrics, when `global.metrics.enabled` is set. |

Point the liveness probe at `/_zendrite/monitor/up` and the readiness probe at `/_zendrite/monitor/health`, both on the `status_address` port, not the Matrix port.
The same paths also exist on the Matrix listener, which a standby never binds.
Probing them there makes every standby fail its probes and restart.
Using the readiness endpoint for liveness has the same effect, because a standby is unready by design.

A candidate reports one of `starting`, `standby`, `fencing`, `recovering`, `active`, `handing-over`, `draining`, or `stopped`.
An elected candidate stays unready through `fencing` and `recovering`, so a Service routes to it only once ownership is established and recovery is complete.
When promotion is blocked, `/_zendrite/monitor/status` names the boundary and the reason.

## Metrics and alerts

All HA metrics use fixed label sets.
Candidate identity and ownership generation are never labels, because they grow without bound across failovers.

| Metric | Meaning |
| --- | --- |
| `zendrite_ha_phase{phase}` | 1 for the current phase, 0 for the others. |
| `zendrite_ha_leadership_transitions_total{transition}` | Leadership acquisitions and losses. |
| `zendrite_ha_renewal_failures_total` | Lease renewal failures, which precede an election. |
| `zendrite_ha_fencing_failures_total{boundary}` | Ownership establishment failures per boundary. |
| `zendrite_ha_recovery_duration_seconds` | Acquisition to readiness for the current activation. |
| `zendrite_ha_shutdown_duration_seconds` | Ownership loss to the end of draining. |
| `zendrite_ha_handover_duration_seconds` | Start of a planned handover to the end of its wait for requests in flight. |
| `zendrite_ha_standby_prepared` | 1 while this standby reaches the database and may take over, 0 otherwise. |
| `zendrite_ha_build_compatible` | 0 when the deployment's upgrade contract doesn't admit this build. |
| `zendrite_ha_build_info{protocol,level,upgrade_from,rollback_to}` | This build's upgrade contract. Always 1. |

Alert on no ready leader, repeated elections, stuck recovery, repeated fencing failures, and a candidate whose build the upgrade contract refuses.
The chart includes these rules (see [Helm setup](/installation/helm/helm/)).

Zendrite sets `zendrite_ha_shutdown_duration_seconds` as the process drains, so a scrape may miss it before the process exits.
Treat it as a best-effort signal, and read the logs for the authoritative shutdown record.

### Broker health

A degraded broker doesn't fail any probe.
These metrics, which every deployment exposes, show it:

| Metric | Meaning |
| --- | --- |
| `zendrite_jetstream_publish_retries_total{stream}` | Publication attempts lost to a broker failure and retried under the same message ID. |
| `zendrite_jetstream_publish_failures_total{stream,reason}` | Publications given up on: `canceled` or `deadline` when the caller stopped waiting, `unavailable` when no broker answered, `refused` when the broker or the ownership barrier rejected it. |
| `zendrite_jetstream_consumer_pending_messages{consumer}` | Messages waiting behind the last one delivered. Also read from the broker every 30 seconds, so it keeps growing when a consumer stops fetching. |
| `zendrite_jetstream_consumer_last_ack_timestamp_seconds{consumer}` | When the consumer last acknowledged a message. |
| `zendrite_jetstream_consumer_fetch_errors_total{consumer}` | Failed pull requests, such as those during a leader election. |
| `zendrite_jetstream_stream_replica_current{stream,replica}` | 1 while the broker `replica` is current with the stream's leader, 0 while it is behind or offline. Read from the broker every 30 seconds. |
| `zendrite_jetstream_stream_replica_lag{stream,replica}` | Entries the broker `replica` is behind the stream's leader, as the leader reports. |
| `zendrite_federationapi_recv_transactions_total{outcome}` | Inbound federation transactions: `ok`, or `retry` when Zendrite refused one because it couldn't queue an event or EDU. |

Set these alerts:

- A consumer has pending messages and hasn't acknowledged one for a few minutes: `zendrite_jetstream_consumer_pending_messages > 0 and time() - zendrite_jetstream_consumer_last_ack_timestamp_seconds > 300`.
- `zendrite_jetstream_publish_failures_total` or the `retry` outcome keeps increasing for more than a few minutes.
- A stream replica stays behind for 10 minutes: `max by (stream, replica) (zendrite_jetstream_stream_replica_current) == 0`.
  The stream still works, but losing one more broker would leave it without a quorum.
  Every candidate exports the same reading, hence the `max`.
  See [A broker replica that stops catching up](#a-broker-replica-that-stops-catching-up).

A short burst of retries and fetch errors during a broker restart or leader election is expected.

## Ownership generation

Election selects a candidate, but it doesn't stop a suspended process from acting when it resumes.
Zendrite therefore records a durable ownership generation in the PostgreSQL table `zendrite_ha_ownership`, next to an append-only `zendrite_ha_ownership_history` table.
Promotion advances the generation with a conditional update.
Two candidates promoting at once therefore serialize, and Zendrite never reuses a retired generation.

Zendrite stores the generation outside the Lease on purpose.
Deleting or recreating the Lease doesn't reset ownership authority.
If the current record is missing while history rows remain, promotion fails closed rather than treating the database as a fresh installation.
Don't delete either table to clear a blocked promotion.
That is an offline bootstrap decision, not a recovery step.

## Database fencing

Every database connection a candidate opens has an `application_name` that identifies its activation.
Zendrite validates it against the recorded generation as it establishes the connection.

When a candidate promotes, it advances the generation.
It then terminates every PostgreSQL backend that belongs to an earlier activation of this homeserver and confirms that none remain.
The sweep terminates any connection opened before the advance.
A connection opened afterwards reads the new generation and refuses itself.
A retired owner therefore can't commit, even if it hasn't noticed that it lost the Lease.

PostgreSQL enforces this, not the application, so it also covers a process that was suspended past its Lease and resumed after takeover.
It depends on every candidate using the same PostgreSQL role, which HA already requires by rejecting per-component database targets.
A transaction that had already flushed its commit before termination is ordered before the promotion.
The successor hasn't activated at that point, so authoritative effects don't overlap.

## Ownership freshness

The database can reject a retired owner by itself, but the message broker can't.
A candidate therefore treats its ownership as valid only while the database has confirmed it recently.

A background heartbeat re-reads the generation on its own guarded connection every `retry_period`.
Ownership stays valid for `renew_deadline` after each confirmation.
A process suspended past its Lease resumes with a stale confirmation, so it refuses to publish or dispatch until it has checked again.
Once a successor has taken over, that check fails.
A candidate that can reach the broker but not the database also goes stale and stops acting, which is the intended fail-closed behavior.

If ownership stays unconfirmed for longer than `lease_duration` after it went stale, the candidate also gives up leadership and exits.
Otherwise a Lease holder partitioned from the database, but not from the Kubernetes API, would keep renewing the Lease.
It would keep every standby out for as long as the partition lasted.
The Lease then expires, and a candidate that can reach the database takes over, roughly `renew_deadline` plus twice `lease_duration` after the partition began.

A candidate tries to take the Lease only after connecting to the database within one `retry_period`.
The restarted former holder, still partitioned, therefore can't win the Lease back and spend `ownership_timeout` failing to fence.
Zendrite doesn't check renewals this way.
A holder that loses the database gives the Lease up as described above.

## Broker fencing

Every JetStream publication passes an ownership check, and Zendrite stamps it with the publishing generation in the `Zendrite-HA-Generation` header.
The connection name includes the activation, so broker monitoring shows an overlapping owner.

Publication is gated on the freshness window, not on a broker-side check.
A retired owner can therefore still publish for a short period after its successor activates.
Consumers count such messages in `zendrite_ha_retired_generation_messages_total` and log them.

Consumers never discard messages based on their generation.
A suspended owner only publishes for writes it already committed before it was fenced, so dropping those messages would lose events that exist in the database.

Once ownership is lost, consumers stop instead of acknowledging.
They check before fetching and again after the callback returns.
Acknowledging work that a retired owner didn't complete would discard it.
Zendrite leaves the message pending so the promoted activation redelivers it.
The roomserver's room workers manage their own subscriptions and apply the same barrier.

Zendrite treats unconfirmed ownership differently from retirement.
A database blip makes a consumer wait and resume.
A worker that exits is never restarted, and would leave a leader that serves traffic while consuming nothing.

## Media, dispatch, and admission

Media backends have no ownership check of their own, so Zendrite refuses a retired owner at the file store.
Uploads, publication, thumbnail replacement, and deletion all fail.
Reads stay open, because serving existing media isn't an authoritative effect, and a draining candidate should still answer in-flight downloads.
A media operation refused for lost ownership returns 503, not 400, so a client retries instead of being told its upload was malformed.

Remote servers can't reject a retired owner either, so Zendrite refuses outbound federation dispatch before a request starts its round trip.
This covers every call that makes a remote server accept an effect on this homeserver's behalf, not only transaction sends:

- Joins, leaves, invites, and knocks.
- Third-party invite exchange.
- One-time key claims, which matter most, because a spent remote one-time key can't be recovered.

A request already dispatched may still complete.
That is the documented at-least-once behavior, and the successor recovers the queue with stable transaction identifiers.

Zendrite distinguishes a refusal from a remote failure, so the destination queue stops instead of recording it.
Charging the refusal to the remote server would persist retry state in a database that the successor inherits.
It would also divert traffic to relays and eventually blacklist servers that were never unreachable.

Appservice and push deliveries begin inside a consumer callback, which the fetch and acknowledgement barriers don't cover.
Each delivery therefore checks ownership right before its HTTP request.

Zendrite evaluates admission per request, not only at readiness.
A request already parked on an established keep-alive connection never passes a new readiness check, so waiting for Kubernetes to remove the endpoint isn't enough.
A candidate that loses ownership rejects client, federation, and admin traffic immediately.

Probe and metrics endpoints are exempt.
Refusing them would blind monitoring and make a candidate look dead to every probe at the moment an operator needs to see why it stopped admitting traffic.

## Kubernetes permissions

The pods' service account needs `get`, `create`, and `update` permissions on `leases` in the `coordination.k8s.io` API group within the configured namespace.

## Prerequisites

- Kubernetes 1.26 or later, for a stable `coordination.k8s.io/v1` Lease API and `PodDisruptionBudget` in `policy/v1`.
- An external HA PostgreSQL with synchronous commit, and an external replicated JetStream.
  HA mode rejects the bundled single-node PostgreSQL and the embedded NATS server.
- Shared media: S3, or an explicitly shared POSIX filesystem.
- The same signing key, server name, and configuration on every candidate, running builds that the deployment's [upgrade contract](#upgrade-contract) admits.
- The same PostgreSQL role for every candidate, which HA already enforces by rejecting per-component database targets.

See [HA storage and recovery](/installation/manual/high-availability/) for the full storage contract.

## Signing keys and bootstrap

Every candidate must present the same signing identity.
Otherwise remote servers reject events depending on which candidate served them.
Mount one signing key Secret into every pod, and don't let each pod generate its own.

Public signing identities are part of the compatibility fingerprint recorded on the Lease.
A candidate with a different key therefore fails closed rather than serving traffic under the wrong identity.
That is a hard stop, not a fallback.
Fix the Secret instead of working around the mismatch.

To bootstrap:

1. Start with `replicaCount: 1` and HA enabled.
2. Confirm that the pod reaches `active`.
   The first candidate creates the annotated Lease and the ownership record.
3. Scale up.

Starting several candidates against an empty database at once is safe but harder to diagnose if a prerequisite is wrong.

## Rolling upgrades

A build that the deployment's upgrade contract admits can replace the running one while clients keep working.
This needs active-only routing that holds requests while no candidate is active and drains a candidate that answers 404.
The Helm chart's `ha.routing.enabled` does this (see [Helm setup](/installation/helm/helm/)).
Without such routing, every handover is a short outage, like a [manual failover](#manual-failover).

### Upgrade contract

Every build declares an upgrade contract instead of pinning its exact executable:

| Field | Meaning |
| --- | --- |
| `protocol` | The ownership, fencing, and compatibility record protocol. Builds of different protocols never share a deployment. |
| `level` | The build's shared-state surface: the PostgreSQL schema and migrations, and the JetStream streams and message formats. It rises with every change to that surface. |
| `upgrade_from` | The oldest level this build can take over from, including the messages such a build left queued. |
| `rollback_to` | The oldest level that may take over from state this build has written. |

Each candidate logs its contract at startup as `HA upgrade contract` and exports it as `zendrite_ha_build_info`.
Candidates must still agree on the deployment's identity: signing keys, server name, database, broker and media targets and namespaces, search settings, and enabled MSCs.
With `msc3861`, the identity includes the OIDC issuer and client.
Changing any of those still needs an [offline upgrade](#offline-upgrades).

The deployment records the level of the build that activated last and a floor in `zendrite_ha_compatibility`.
The floor is the highest `rollback_to` of every build that has activated.
Storage keeps every migration a build ran, so the floor only rises.
A candidate may take over when these conditions hold:

- Its protocol matches.
- Its level is at or above the floor.
- If its level is above the recorded one, its `upgrade_from` is at or below the recorded level.

Zendrite checks the contract three times, and never by writing storage that the running owner uses:

- A standby checks the record every `retry_period`.
  It is a healthy candidate on `/_zendrite/monitor/candidate` only while the record admits it and it reaches the database.
  An incompatible image therefore never becomes Ready, and a rollout stalls on it while the old candidates keep serving.
- The candidate checks again before every attempt to take the Lease, so an incompatible build never retires the running owner, even when the owner has released the Lease.
- After fencing, activation checks once more under the migration lock and records its own level before any migration runs.

`/_zendrite/monitor/status` reports the contract, the deployment's level, floor, and last activated version, and whether the standby is prepared:

```json
{"phase": "standby", "upgrade": {"version": "3.5.0", "protocol": 1, "level": 2, "upgrade_from": 1, "rollback_to": 1, "deployment_level": 1, "deployment_floor": 1, "deployment_version": "3.4.0", "prepared": true, "compatible": true}}
```

A refused standby logs `HA standby is not prepared to take over` with the reason, reports `compatible: false`, and sets `zendrite_ha_build_compatible` to 0.

### Supported upgrades

`setup/ha/testdata/levels.json` in the source repository records the upgrade matrix, one entry per level.
CI refuses a change to the shared-state surface that doesn't declare a new level.

| Level | Introduced | Upgrades from | Rolls back to |
| --- | --- | --- | --- |
| 1 | first release after 3.3.0 | 1 | 1 |

Builds of the same level are always interchangeable, so patch releases and rebuilds roll without restrictions.
A new level lists how far back it can upgrade from.
Unless its `upgrade_from` says otherwise, only adjacent levels are supported, and skipping a level needs an intermediate rollout or an offline upgrade.
Earlier releases pinned their exact executable and have no contract, so reaching level 1 is one last offline upgrade.

These changes still need an offline upgrade:

- Anything in the deployment identity, including enabling or disabling an MSC, changing the topic prefix, or moving media.
- A level whose `upgrade_from` excludes the running level, for example a stream storage, retention, or subject change, which HA refuses to apply to an existing stream.
- A rollback below the floor.

### Runbook

1. Read the new release's level, `upgrade_from`, and `rollback_to` in its release notes or `levels.json`.
   Compare them with `deployment_level` and `deployment_floor` in the status of a running candidate.
2. Back up the database, as for any upgrade that may run migrations.
3. Roll out the new image with `strategy.type: RollingUpdate`, `maxSurge: 0`, and `maxUnavailable: 1`.
   Each replacement pod must become Ready before Kubernetes stops the next old pod.
   For a standby, Ready means prepared.
4. Watch `kubectl rollout status`.
   If the rollout stalls on an unready new pod whose status reports `compatible: false`, you need the offline procedure.
   Undo the rollout with `kubectl rollout undo`, which leaves the running candidates untouched.
5. When the rollout stops the active candidate, it hands over and a prepared candidate takes over.
   The successor logs `HA deployment level changed` when it activates a new level.

### Planned handover

When a rollout or a pod deletion stops the active candidate, the candidate hands over instead of stopping at once:

1. `/_zendrite/monitor/health` answers 404.
   Routers send the candidate no new requests but keep the ones in flight, and hold new requests until a candidate is active.
2. Long-polling `/sync` and sliding sync requests answer at once with the client's own position.
   The client's next request waits for the successor.
3. The candidate keeps admitting requests for `handover_grace`, which must cover the routers' health check interval.
   It then waits for every request in flight, for at most `handover_timeout` in total.
4. It stops its consumers and background work, releases the Lease, and exits.
   A prepared standby takes the Lease at its next `retry_period`, fences, runs any migrations, and becomes active.
   The routers then release the held requests to it.

Clients see a pause of a few seconds instead of errors.
The pause is the grace period, the retry period, and the successor's activation.
`ha.terminationGracePeriodSeconds` must cover `handover_timeout` plus the 30-second shutdown budget.
Set `handover_timeout: 0s` to stop at once instead.

Some state is process-local and doesn't survive a handover, as after any takeover:

- Search answers HTTP 503 with `Retry-After` until the successor has rebuilt its index, which takes time in proportion to the searchable history.
- Sliding sync connections start over, and typing notifications are dropped.
- A login, SSO, or user-interactive authentication flow in progress must restart.
  With delegated authentication (`msc3861`), this includes a legacy SSO login whose login token wasn't yet redeemed.
  It also includes a cross-signing reset that the provider confirmed but the client hadn't yet completed: the user confirms the reset again.
- The successor retries outbound federation, appservice, and push deliveries that were in flight when the owner stopped, with their original transaction IDs.

A rollout under `maxSurge: 0` may stop the active candidate before the standbys, so it can hand over twice: once to an old standby and once to a new one.
Each handover is uninterrupted.

### Aborting and rolling back

Before the active candidate is replaced, undoing the rollout is always safe, because no new build has written anything.
If the successor fails while it activates, its Lease expires and another prepared candidate takes over, as in an ordinary takeover.
Clients may see errors once the routers' hold runs out.

Once a new level has activated, its migrations have run.
Rolling back to the old image is a rolling upgrade in reverse.
Zendrite supports it only while the old level is at or above the deployment's floor, which is the new build's `rollback_to`.
Below the floor, Zendrite refuses old candidates before they take the Lease, so a rollback leaves the new build serving.
Restoring the old version then needs the offline procedure and the database backup.

### Offline upgrades

When the contract doesn't admit the new build, upgrade in a maintenance window:

1. Scale to zero.
2. Confirm that no pod is running.
3. Reset both compatibility records.
4. Scale back up on the new version.

[HA storage and recovery](/installation/manual/high-availability/#offline-upgrades) describes the full procedure.

## Manual failover

Delete the pod that reports `active`.
It [hands over](#planned-handover) like the active candidate in a rolling upgrade.
It drains its requests in flight, then releases the Lease, so a standby takes over at its next `retry_period` instead of after `lease_duration`.
A leader that crashes, or doesn't drain within the shutdown budget, leaves the Lease to expire instead.

Don't delete the Lease to force a failover.
The Lease is coordination, not authority, and deleting it doesn't retire the ownership generation.

With routing that holds requests, as the chart's does, clients see a pause instead of errors, apart from the process-local exceptions listed under [Planned handover](#planned-handover).
Without such routing, expect a visible interruption:

- Clients receive connection errors or `503` responses until the successor is ready, then reconnect and resume from their existing sync tokens.
- After an unplanned takeover, Zendrite drops long-lived `/sync` connections, and clients must re-establish them.
- Typing notifications, login and SSO intermediate state, and sliding sync sessions are process-local, so clients may need to retry a request or start a fresh session.

Retrying a message with the same transaction ID is safe across a takeover.
The roomserver remembers which event each transaction produced for 24 hours.
It answers a retry with the event that the first attempt stored, even if the old owner stored it.

## Rolling back to a single instance

Scale to `replicaCount: 1` and wait until exactly one candidate reports `active`.

To leave HA entirely:

1. Scale to zero.
2. Confirm that no pod is running.
3. Set `ha.enabled: false`.
4. Start one instance with the same identity and storage.

Don't set `ha.enabled: false` while several pods are running.
Without election, nothing prevents two active homeservers.

## Recovering from blocked fencing

A candidate that can't establish ownership refuses to activate and exits with a nonzero status.
Kubernetes restarts it, and it contends again.
This is intended: it fails closed rather than risk two active homeservers.

Read the blocking reason from the candidate's logs.
`/_zendrite/monitor/status` reports the boundary only in the brief window before the process exits.
For the same reason, a scrape is likely to miss `zendrite_ha_fencing_failures_total`, so neither is a reliable record of a single failure.
`ZendriteHANoReadyLeader` and the pod's restart count show a candidate stuck in a restart loop.

Look at the named boundary first.
A `postgresql` block usually means a backend of the previous activation is still connected, and the status reason reports the count.
Find them with `SELECT pid, application_name, state FROM pg_stat_activity WHERE application_name LIKE 'zendrite:%'`, and confirm that the old pod is gone.
Don't delete the ownership tables to clear the block.
That discards the evidence that makes takeover safe.

## Guarantees and limitations

Supported:

- One active homeserver at a time, enforced at the database, broker, media, dispatch, and admission boundaries.
- Automatic takeover after a candidate loses its Lease, provided Zendrite can establish ownership.
- Committed PostgreSQL writes and acknowledged JetStream publications survive takeover under the documented storage settings.
- [Rolling upgrades](#rolling-upgrades) between builds that the upgrade contract admits, and planned handovers, without failed client requests, apart from the documented process-local exceptions.

Not supported, and not planned for this iteration:

- Active-active.
  Only one candidate serves traffic, and the others are cold.
- Zero-downtime failover after an unplanned failure.
  Takeover restarts the homeserver and rebuilds process-local state.
  Routing that holds requests only hides this while the takeover is shorter than the hold.
- Rolling upgrades that the contract doesn't admit, and automatic schema downgrade.
- Exactly once external delivery.
  Federation, appservice, and push retries can duplicate remote deliveries.
  See [Inbound federation delivery](#inbound-federation-delivery) for what an inbound transaction guarantees.
- Election backends other than Kubernetes.

The [qualification report](/development/ha-qualification-report/) measures recovery against the ADR reference workload.
A successor opens its listeners without waiting for its search index, so the size of the search history doesn't delay takeover.
Search answers HTTP 503 until the rebuild finishes, which takes time in proportion to the searchable history.
One production candidate rebuilt 84,000 messages in about 32 seconds.
Each activation logs `Activation phase finished` with the duration of every startup phase, and `Search index rebuilt` with the duration of the rebuild.

## Failover behavior

Standbys don't serve traffic until takeover and startup are complete.
An instance that loses leadership stops accepting requests, shuts down, and exits.
If a standby can't confirm a safe takeover, it doesn't start serving traffic.
Check the application logs for election and startup failures.

### Candidate health and active-only routing

`/_zendrite/monitor/candidate` returns HTTP 200 for a prepared standby or an active candidate.
A standby is prepared once it has checked that it reaches the database and that the deployment's upgrade contract admits its build.
Starting, fencing, recovering, draining, stopped, and failed candidates return HTTP 503.
`/_zendrite/monitor/health` remains the active-only traffic check.

Use the candidate endpoint for Kubernetes readiness only with a separate routing layer that checks the active-only endpoint before it forwards Matrix traffic.
The Helm chart provides this with `ha.routing.enabled`, using replicated HAProxy routers and DNS discovery of all candidate status endpoints.
Healthy standbys then become Ready without joining Matrix traffic directly, and applications need no permission to change routing resources.

Failover includes Lease expiry, election retry jitter, fencing, recovery, and routing convergence.
Shorter Lease and retry periods can reduce detection time.
They also make leadership more sensitive to control plane stalls and increase renewal traffic (see [Choosing election timings](#choosing-election-timings)).
Shorter election timings never skip resource fencing or backlog recovery.

## Suspended owners and broker delivery

HA pull requests expire after one second, and unacknowledged durable deliveries become eligible for redelivery after five seconds.
A healthy long-running handler extends its delivery once per second while its ownership remains valid.
A successor can therefore recover work from a suspended process without waiting for that process to resume or deleting durable consumers.
Workers recheck ownership after fetching.

Retired room workers must not acknowledge or answer a successor's synchronous request.
Delivery timing without HA is unchanged.

### Recovery while a broker is unavailable

Configure each broker address separately, so the client can try another broker when an initial connection fails.
A single headless service address can resolve to an unresponsive broker before the client has discovered its peers.
Activation needs an elected leader and a current majority for existing replicated streams.
Creating a stream or changing its replica count still waits for every configured replica.
No recovery path reduces the configured replica count or deletes queued data.

Active traffic and candidate probes check fencing validity as well as lifecycle state, so a resumed retired owner can't re-enter routing while Lease renewal is still timing out.

A broker whose host stops responding keeps its connections open, so nothing tells the client that the broker is gone.
Zendrite pings its broker every 10 seconds and reconnects to another one after two unanswered pings.
A silent broker therefore costs about 30 seconds of failed publications instead of the four to six minutes of the NATS client defaults.
No takeover is needed: the owner keeps its ownership and resumes once it reaches a broker that has a current majority.

A broker that rejoins, on its old storage or on an empty volume, catches up from its peers.
Check that every stream and consumer replica is current before you take another broker down.

### A broker replica that stops catching up

A broker that rejoined on its retained storage can stop applying a stream, for example after its node was NotReady and the pod was replaced ([#473](https://codefloe.com/pat-s/zendrite/issues/473)).
The stream keeps its leader and a current majority, so Zendrite keeps working and loses nothing.
Losing one more broker, though, would leave the stream without a quorum.
The broker doesn't recover by itself.

This is a regression in nats-server 2.15 that occurs only with `sync_interval: always`, which HA requires ([nats-io/nats-server#8709](https://github.com/nats-io/nats-server/issues/8709)).
It affects 2.15.0 and 2.15.1-RC.1, but not 2.14 releases.
nats-server 2.15.1 fixes it ([nats-io/nats-server#8710](https://github.com/nats-io/nats-server/pull/8710)), so run your brokers on 2.15.1 or later.

To recognize it:

- `zendrite_jetstream_stream_replica_current` stays 0 for one broker and one or more streams while that broker is running.
- Zendrite logs `JetStream stream replica is not catching up with its leader` with the stream and the broker after five minutes, and `JetStream stream replica caught up with its leader` once it recovers.
- The broker logs `critical write error: expected sequence does not match store` for the stream, and its full `/healthz` on the monitoring port reports a `stream write error`.

To recover:

1. Restart that broker, for example by deleting its pod, and let it start on the same volume.
   The restarted broker reloads the stream from its storage and catches up from the leader within seconds.
2. Check that `zendrite_jetstream_stream_replica_current` returns to 1 for every stream.

Restart one broker at a time, and only while the other brokers are current.
You don't need to wipe its volume.
`nats stream cluster peer-remove` doesn't help, because a three-broker cluster has no spare broker to move the replica to.

### Inbound federation delivery

Zendrite tells a remote server that a transaction succeeded only once it has queued the contents in the broker.
The guarantees depend on what the transaction contains:

- **Events:** Zendrite reports an event that fails its checks in the response, as the specification requires.
  An event that Zendrite couldn't queue fails the whole transaction with HTTP 503, so the sender retries it.
  The roomserver ignores events it already has.
- **To-device messages, device list updates, and signing key updates:** One that Zendrite couldn't queue also fails the transaction with HTTP 503.
  Zendrite deduplicates retried to-device messages by the sender's `message_id` for two minutes, the broker's deduplication window.
  A later retry can deliver a message twice, which clients already tolerate.
- **Read receipts:** Receipts are also retried this way, but they coalesce.
  A newer receipt from the same user replaces an older one, so only the newest is sure to become visible.
- **Typing notifications and presence:** These are best effort and expire within seconds.
  Zendrite drops one that it couldn't queue instead of making the sender resend the transaction.

Zendrite processes an accepted transaction to completion for up to two minutes, even if the sender disconnects.
A sender that times out and retries the same transaction receives the outcome of the attempt still running, not a partial result.

A retry joins a running attempt only when it has the same events and EDUs.
Older Zendrite and Dendrite senders reuse the transaction ID with more content.
Zendrite processes such a retry on its own.
Zendrite itself sends a retry under a new transaction ID whenever its content changed, because a destination may answer a known ID with its earlier outcome.

Zendrite logs each refused transaction as `Refusing federation transaction for a retry` and counts it in `zendrite_federationapi_recv_transactions_total{outcome="retry"}`.
