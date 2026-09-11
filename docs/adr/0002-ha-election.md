# ADR 0002: Coordinate initial HA with Kubernetes Leases

Status: Proposed for maintainer review; implementation belongs to #348.
Related: #345, #346.

## Decision

Use the ordinary Go client-go leader-election library and coordination.k8s.io Lease objects for Kubernetes deployments.
Do not implement a custom consensus algorithm or use the experimental coordinated-election API.
Choose a client-go version compatible with the project's Go version and supported Kubernetes versions at implementation time.
Each candidate has a unique process identity, pod UID, node UID, and provider instance identifier.
Each homeserver has one stable Lease name and namespace shared by every candidate.
Persist the previous owner's infrastructure identity outside the expiring Lease so deleting a Lease cannot erase the fencing obligation.

An acquired Lease permits the candidate to request fencing verification, not to start the homeserver.
Bind the activation context to continued ownership.
Loss during fencing, migration, or recovery cancels promotion.
Only release a Lease voluntarily after protected work has stopped and the required fencing/drain conditions are satisfied.
A failed renewal triggers local shutdown, but ADR 0003 remains mandatory before another owner starts.

Configure and validate Lease duration, renewal deadline, and retry period with the library's required ordering and jitter constraints.
Use 30 seconds, 20 seconds, and five seconds respectively as initial reference values, then validate them under control-plane latency and fault tests.
These values tune availability; they do not prove exclusive execution.
Use least-privilege namespaced Lease access, including only the read/create/update permissions needed by the selected client-go implementation.
Fail clearly on missing permissions or ambiguous identities.

## Alternatives

| Backend | Benefits | Costs and limitations |
| --- | --- | --- |
| Kubernetes Lease | Established Go implementation; observable through Kubernetes; fits the existing Helm deployment | Control-plane dependency; requires external fencing; callback cancellation cannot stop a paused former leader |
| PostgreSQL advisory lock | Available with the existing database; useful outside Kubernetes | Dedicated connection and reconnect discipline; losing the lock does not stop writes on other connections, NATS publication, or remote sends; database failover complicates lock ownership |
| Custom distributed lock | Potentially fewer dependencies | Adds an unnecessary correctness burden and still does not fence external effects |

Select Kubernetes first and defer a second backend.
Do not add Kubernetes dependencies or contact its API when HA is disabled.

## Reference

The [client-go leader-election documentation](https://pkg.go.dev/k8s.io/client-go/tools/leaderelection#pkg-overview) explicitly states that election does not guarantee exclusive execution.
Its Lease release and callback behavior must be included in implementation review.
