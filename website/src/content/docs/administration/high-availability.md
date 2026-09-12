---
title: High availability development status
description: Opt-in Lease election, ownership requirements, and current limitations.
---

Kubernetes Lease election is an experimental foundation for active-passive Zendrite deployments.
**Automatic HA activation is unavailable until the enforceable fencing provider in issue #349 is implemented.**
The binary refuses application startup after election when no fencing provider is installed.
Leave HA disabled for normal deployments.

## Configuration

The optional `global.ha` section defaults to disabled.
Disabled HA requires no Kubernetes service account, API access, or election.

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
```

All candidates for one homeserver must use the same Lease name and namespace.
Use a different Lease scope for each homeserver.
Set `candidate_id` to the pod UID through deployment configuration; the process appends a fresh UUID, including after container restarts.
Configuration values are literal strings, with no environment-variable interpolation.
The initial backend uses in-cluster service-account credentials only.
The implementation pins client-go v0.37.0, which requires Go 1.26, matching Zendrite.
It uses the stable `coordination.k8s.io/v1` Lease API and ordinary leader election, without coordinated-election feature gates.
Kubernetes deployment qualification remains part of issues #351 and #352; this change does not establish a tested cluster-version support matrix.

`lease_duration` must be at least one second and greater than `renew_deadline`.
`renew_deadline` must exceed 1.2 times the positive `retry_period`, matching client-go's jitter constraint.
These timings govern detection and availability, not exclusive execution.

## Permissions and failure behavior

The service account needs namespaced `get`, `create`, and `update` permission on `leases` in API group `coordination.k8s.io`.
For least privilege, precreate the Lease and grant `get` and `update` on that specific resource name; creation is then unnecessary.
The election implementation does not need list, watch, patch, delete, or event permissions.
Denied access produces an error identifying the Lease and required permissions, then terminates the candidate.
Transient API errors use client-go retries: a standby stays alive without starting the application, while a leader that cannot renew loses ownership and exits.
Ordinary contention keeps the unsuccessful candidates alive and inert.

## Ownership and shutdown contract

Acquisition only permits calling `OwnershipGate.Acquire` with the unique candidate identity and Lease scope.
The provider must confirm enforceable fencing, including durable previous-owner identity outside the Lease, before returning success.
The missing provider fails closed with an actionable error.
A successful gate is followed by application recovery; election callbacks never directly initialize the homeserver or publish readiness.
Cancellation during fencing or recovery closes local admission and prevents readiness.
Application shutdown and fencing cancellation have a bounded completion budget; exceeding it requires immediate process termination.
Leadership loss is terminal even after successful draining, with no reacquisition in the same process.

`ReleaseOnCancel` is explicitly false.
A signal, renewal failure, or interrupted promotion must not voluntarily release leadership while protected work may still be active.
The Lease remains recorded until expiry or takeover, even after successful draining.
Expiry, pod deletion, readiness changes, and local cancellation do not establish fencing.
Every successor must independently satisfy the ownership gate, including when a Lease was deleted or recreated.
See the [client-go election contract](https://pkg.go.dev/k8s.io/client-go@v0.37.0/tools/leaderelection) and the proposed ADRs in [issue #346](https://codefloe.com/pat-s/zendrite/issues/346).

External fencing, durable recovery validation, deployment probes, and failure qualification remain tracked in issues #349 through #352.
This election foundation does not promise protection from a paused former leader or exactly-once remote delivery by itself.
