---
title: Helm Setup
description: Installing Zendrite on Kubernetes with Helm
---

Install Zendrite using the [devxy/helm-zendrite](https://codefloe.com/devxy/helm-zendrite) Helm chart.

## Installation

Using OCI:

```bash
helm install zendrite oci://codefloe.com/devxy/zendrite
```

Or using a classic Helm repository:

```bash
helm repo add codefloe.com https://codefloe.com/api/packages/devxy/helm
helm install zendrite codefloe.com/zendrite
```

## Configuration

Create a `values.yaml` file and set at least `server_name`:

```yaml
zendrite_config:
  global:
    server_name: "localhost"
```

The [chart README](https://codefloe.com/devxy/helm-zendrite/src/branch/main/charts/zendrite/README.md) lists all values.

To use an existing PostgreSQL database, configure the connection:

```yaml
zendrite_config:
  global:
    database:
      connection_string: "postgresql://PostgresUser:PostgresPassword@PostgresHostName/ZendriteDatabaseName"
      max_open_conns: 90
      max_idle_conns: 5
      conn_max_lifetime: -1
```

## Element Call

The chart can deploy the backend that [Element Call](/administration/matrixrtc/#deploying-element-call) needs.
It runs two LiveKit nodes and two lk-jwt-service replicas, which share their state through an external Valkey or Redis.
The chart adds this backend to `rtc_foci` and enables delayed events for up to 24 hours.
It serves LiveKit and lk-jwt-service on a separate host through the Ingress or HTTPRoute the chart already creates.

```yaml
matrixRTC:
  enabled: true
  hostName: rtc.example.com
  livekitAuth:
    # a Secret with an API secret of at least 32 characters
    existingSecret: livekit
  redis:
    # an HA Valkey, for example the valkey-io chart with Sentinel and HAProxy
    address: valkey-haproxy.valkey.svc:6379
    existingSecret: valkey
```

Each LiveKit pod runs with host networking on a dedicated node.
Clients send media straight to it on 7881/TCP and 7882/UDP, so open these ports on every node that may run LiveKit.
A namespace that enforces the `baseline` Pod Security level rejects host networking and needs `privileged`.
A stopping LiveKit pod serves its running calls until they end, for up to five hours, so a rollout waits for each node's calls.
The [chart README](https://codefloe.com/devxy/helm-zendrite/src/branch/main/charts/zendrite/README.md) lists all `matrixRTC` values.

## High availability

The chart can run several candidates with one ready, safely active homeserver.
HA is experimental.
Read [High availability](/administration/high-availability/) and [HA storage and recovery](/installation/manual/high-availability/) before you enable it.

The values below require the chart release that adds HA support, tracked in [devxy/helm-zendrite#35](https://codefloe.com/devxy/helm-zendrite/pulls/35).
Earlier releases have no `ha` or `replicaCount` values, no Lease RBAC, and no status Service.
If you enable HA on them, pods lack permission to acquire their Lease.

```yaml
replicaCount: 3

ha:
  enabled: true

# maxSurge must be 0: a surged candidate cannot become Ready while the old one
# still owns the homeserver, so the rollout would wait for it forever.
strategy:
  type: RollingUpdate
  rollingUpdate:
    maxSurge: 0
    maxUnavailable: 1
```

Kubernetes then restarts candidates one at a time and a standby takes over, which suits a configuration change or a same-version restart.

A **version change is different**.
Candidates pin themselves to one executable fingerprint recorded on the Lease, so new-version pods cannot acquire it and the deployment ends with no owner at all.
Use the maintenance window described in [High availability](/administration/high-availability/#offline-upgrades) for any version change.

The chart rejects `replicaCount` above 1 without `ha.enabled`, because running several active replicas without leader election corrupts shared state.
The chart also rejects a rolling update with a nonzero `maxSurge`.
A surged candidate can't be promoted while the old one still has ownership, so the rollout would deadlock.

The chart creates a service account with `get`, `update` and `create` on the homeserver's Lease, a headless status Service so standbys can be scraped, a disruption budget, and alert rules.
It does not provision the HA prerequisites.
Point the chart at an external HA PostgreSQL, an external replicated JetStream, and shared media, and disable the bundled PostgreSQL.

## Installing with PostgreSQL

The chart can install PostgreSQL alongside Zendrite as a dependency.
Enable it in your `values.yaml`:

```yaml
postgresql:
  enabled: true # this installs Postgres
  primary:
    persistence:
      size: 1Gi # defines the size for $PGDATA

zendrite_config:
  global:
    server_name: "localhost"
```

The chart then sets `database.connection_string` for you.
