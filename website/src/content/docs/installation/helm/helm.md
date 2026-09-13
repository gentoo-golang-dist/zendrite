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

Create a `values.yaml` file and configure it to your liking.
All possible values can be found in the [chart README](https://codefloe.com/devxy/helm-zendrite/src/branch/main/charts/zendrite/README.md), but at least you need to configure a `server_name`:

```yaml
zendrite_config:
  global:
    server_name: "localhost"
```

If you are going to use an existing Postgres database, you'll also need to configure this connection:

```yaml
zendrite_config:
  global:
    database:
      connection_string: "postgresql://PostgresUser:PostgresPassword@PostgresHostName/ZendriteDatabaseName"
      max_open_conns: 90
      max_idle_conns: 5
      conn_max_lifetime: -1
```

## High availability

The chart can run several candidates with one ready, safely active homeserver.
HA is experimental; read [High availability](/administration/high-availability/) and [HA storage and recovery](/installation/manual/high-availability/) before enabling it.

```yaml
replicaCount: 3

ha:
  enabled: true

strategy:
  type: RollingUpdate
  rollingUpdate:
    maxSurge: 0
    maxUnavailable: 1
```

`replicaCount` above 1 without `ha.enabled` is rejected, because running several active replicas without leader election corrupts shared state.
A rolling update with a nonzero `maxSurge` is rejected as well: a surged candidate cannot be promoted while the old one still holds ownership, so the rollout would deadlock.

The chart creates a service account with `get`, `update` and `create` on the homeserver's Lease, a headless status Service so standbys can be scraped, a disruption budget, and alert rules.
It does not provision the HA prerequisites.
Point the chart at an external HA PostgreSQL, an external replicated JetStream, and shared media, and disable the bundled PostgreSQL.

## Installing with PostgreSQL

The chart comes with a dependency on Postgres, which can be installed alongside Zendrite. Enable it in your `values.yaml`:

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

Using this option, the `database.connection_string` will be set for you automatically.
