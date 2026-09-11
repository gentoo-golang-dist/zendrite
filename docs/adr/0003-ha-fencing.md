# ADR 0003: Require confirmed infrastructure fencing before takeover

Status: Proposed for maintainer review; the provider integration and fault proof are release gates in #349 and #352.
Related: #345, #346.

## Context

A leader may be suspended beyond its lease or lose Kubernetes API access while retaining PostgreSQL, NATS, and federation connectivity.
When it resumes, request middleware and a cancellation callback may not run before an already-started worker continues.
A local timer, Kubernetes readiness, pod deletion request, or an advisory lock on a separate connection cannot establish safe takeover.

## Decision

Use a separate infrastructure fencing controller with authority to stop the former owner's virtual machine or physical node through its provider or hypervisor management API.
Fence the whole application node, not merely its Kubernetes pod.
Place candidates on distinct dedicated application nodes and keep PostgreSQL, NATS, media services, and the fencing controller outside those nodes.
This avoids destroying shared dependencies or unrelated applications during a fence.

The fencer accepts a homeserver identity, previous process identity, immutable node/provider identity, and requested ownership generation.
It returns durable evidence tied to those exact identities and generation only after the management plane confirms the old instance is powered off and latched against automatic restart.
An accepted power-off request, node NotReady condition, deleted pod, or elapsed timeout is insufficient evidence.
Unknown identity, ambiguous owner history, fencer outage, and provider timeout all fail closed.
A replacement cannot authorize itself by deleting the old owner's record.

Keep a durable ownership ledger with conditional updates in the fencing controller.
Record the next owner before authorizing application activation, including first bootstrap.
Bootstrap is explicit and succeeds only when the ledger proves there is no previous activation requiring fencing.
Protect the ledger against concurrent requests and preserve old ownership obligations across controller restart.
A candidate verifies that its own Lease ownership still holds after fencing completes and before activation.
No candidate may power its predecessor back on or clear the fencing latch.
A separate operator recovery procedure resets the former node only after removing its old process state and credentials/assignment as required, so it returns as a fresh standby.

Give Zendrite permission to request fencing and read evidence only for its own homeserver.
Only the fencing controller receives provider power-control and restart-latch permissions for the dedicated application nodes.
Authenticate requests and audit requests, provider responses, immutable instance IDs, generations, and recovery actions without logging credentials.
Use a 45-second fencing request budget in the reference deployment; timeout leaves the successor unready and raises an alert.
The selected provider adapter must demonstrate power-off confirmation and a persistent restart latch in #349.
Deployments without those capabilities are not supported for strict automatic HA.
The initial implementation must not silently substitute a weaker pod-deletion adapter.

## Drain already-dispatched operations

Power-off prevents the former process from issuing further operations; it does not undo requests already accepted by a remote service.
Before promotion, terminate or drain the old application's PostgreSQL sessions, confirm transaction completion/rollback, and ensure no old backend can commit after the database barrier.
Tag every application connection with the activation identity so the barrier can cover all pools.
Use equivalent NATS client identity and disconnect/drain evidence for old publications and acknowledgements.
The fencer's promotion evidence includes these data-plane barriers, not just power state.
The implementation must define and test provider/database/broker-specific barriers; if a barrier cannot be established, promotion remains blocked.

Media object writes already accepted before fencing may finish afterward.
Require immutable or idempotent object keys and make PostgreSQL metadata the publication authority; do not allow stale mutable media deletion/overwrite paths without an equivalent barrier.
Audit these assumptions in #350 before permitting the backend in HA mode.

Federation, appservice, and push requests already dispatched may be delivered after power-off and cannot be recalled.
Recover durable queues using protocol transaction IDs and existing idempotency where supported.
Document possible duplicate remote delivery and do not claim exactly-once behavior.
No new send can originate from the fenced process, but remote completion of an earlier send is permitted.

## Failure envelope and trade-offs

SIGSTOP/SIGCONT cannot revive a powered-off instance that remains latched off.
An application-to-Kubernetes partition cannot prevent the independent management plane from fencing it.
If management connectivity is also lost, automatic takeover waits rather than risking overlapping owners.
Even graceful failover uses the same proof unless the fencer can attest process termination and the data-plane barriers through an equally strong provider-specific mechanism.

This design costs dedicated nodes, infrastructure privileges in a separate controller, and slower failover.
It deliberately gives up availability when fencing is uncertain.
Atomic resource fencing across PostgreSQL, NATS, object storage, and arbitrary external HTTP destinations would require a much larger redesign, so it is not selected for the initial release.

## Release evidence

Test a paused old process, asymmetric partitions, stale or replayed fencing responses, a restarted fencing controller, missing owner history, provider power-off failure, blocked database sessions, and late broker operations.
Assert that the old activation cannot issue new authoritative work or commit stale database transactions after promotion.
Measure takeover against ADR 0001's reference recovery target and separately record remote retries already in flight.
