---
title: Profiling
description: Profile CPU and memory use with pprof and Grafana Pyroscope.
---

When Zendrite uses too much CPU or RAM, profile it to find out why.
Zendrite embeds `pprof`, the profiler from the standard Go toolchain.

## Continuous profiling with Grafana Pyroscope

Zendrite can continuously push CPU and memory profiles to a Pyroscope server or Grafana Cloud Profiles.
Profiling is off by default.
To turn it on, configure `global.pyroscope` in `zendrite.yaml` and restart Zendrite:

```yaml
global:
  pyroscope:
    enabled: true
    application_name: zendrite
    server_address: http://localhost:4040
    basic_auth:
      username: ''
      password: ''
    tenant_id: ''
    tags:
      instance: my-homeserver
```

Set `server_address` to your Pyroscope ingestion URL.
The Zendrite process or container must be able to reach it.
For Grafana Cloud Profiles, take the URL, username, and access policy token from your stack's Profiles configuration, and put the token in `basic_auth.password`.

Set `tenant_id` only if your Pyroscope server requires multi-tenancy.
The `basic_auth`, `tenant_id`, and `tags` settings are optional, and `application_name` defaults to `zendrite`.
See the [Grafana Go SDK documentation](https://grafana.com/docs/pyroscope/latest/configure-client/language-sdks/go_push/) for backend configuration details.

Zendrite collects CPU samples, allocated objects and bytes, and in-use objects and bytes.
It uploads profiles in the background and flushes the remaining data during graceful shutdown.
Zendrite logs upload failures and keeps running.
In Grafana, select the configured application name to explore its profiles, and filter by your tags.

Pyroscope doesn't need `PPROFLISTEN`.
While continuous profiling is on, don't request a CPU profile from the pprof endpoint, because both use Go's single CPU profiler.
To collect a manual CPU profile, turn Pyroscope off and restart Zendrite first.

## Enable the pprof profiler

Start Zendrite with the `PPROFLISTEN` environment variable set to the address and port to listen on:

```bash
PPROFLISTEN=localhost:65432 ./bin/zendrite ...
```

At startup, Zendrite logs that pprof is listening:

```text
level=warning msg="Starting pprof listener on localhost:65432 — this endpoint has NO authentication, do not expose to untrusted networks"
```

The examples below assume `PPROFLISTEN=localhost:65432`.
Adjust them for your setup.

## Profile CPU usage

To see where CPU time goes, call the `profile` endpoint:

```text
http://localhost:65432/debug/pprof/profile?seconds=30
```

The profile runs for the given number of `seconds`, then returns a result.

### Examine a CPU profile with the Go toolchain

If you have Go installed, open the profile with `go tool pprof`.
The `-http=` parameter starts a web server that shows the captured profile:

```bash
go tool pprof -http=localhost:23456 http://localhost:65432/debug/pprof/profile?seconds=30
```

Then visit `http://localhost:23456` in your web browser to see the profile.
In the "View" menu, select "Flame Graph" for a proportional, interactive graph of CPU usage.

### Download a CPU profile to send to someone else

Without the Go tools, download the profile with `curl`:

```bash
curl -O http://localhost:65432/debug/pprof/profile?seconds=30
```

The command blocks for the given number of seconds while it captures what Zendrite is doing.
It then writes a `profile` file that you can send onward.

## Profile memory usage

To see where memory goes, call the `heap` endpoint:

```text
http://localhost:65432/debug/pprof/heap
```

The profile returns almost instantly.

### Examine a memory profile with the Go toolchain

If you have Go installed, open the profile with `go tool pprof`.
The `-http=` parameter starts a web server that shows the captured profile:

```bash
go tool pprof -http=localhost:23456 http://localhost:65432/debug/pprof/heap
```

Then visit `http://localhost:23456` in your web browser to see the profile.
The "Sample" menu selects one of four memory profiles:

- `inuse_space`: how much heap memory each function has allocated and still in use.
  This is usually the most useful profile for diagnosing high memory usage.
- `inuse_objects`: how many heap objects each function has allocated and still in use.
- `alloc_space`: how much memory each function has allocated in total, including memory since freed.
- `alloc_objects`: how many allocations each function has made in total, including memory since freed.

In the "View" menu, select "Flame Graph" for a proportional, interactive graph of memory usage.

### Download a memory profile to send to someone else

Without the Go tools, download the profile with `curl`:

```bash
curl -O http://localhost:65432/debug/pprof/heap
```

The command writes a `heap` file almost instantly, which you can send onward.
