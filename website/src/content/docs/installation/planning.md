---
title: Planning your installation
description: Database choices, system requirements, and dependencies for Zendrite
---

Before you install Zendrite, choose a database, size your server, and install the dependencies.
Then [set up your domain name](/installation/domainname).

## Database

Zendrite stores its data in PostgreSQL or SQLite.
Use **PostgreSQL for all production deployments**.

- **PostgreSQL**: runs as a separate service that you install and configure yourself.
  It uses more resources overall, but it's **considerably faster** than SQLite.
  PostgreSQL handles concurrent writes much better, so Zendrite can process more tasks in parallel.
  Federated deployments need this to perform adequately.

- **SQLite**: built into Zendrite, so you don't install a separate database engine, and setup is easier.
  It's usually much slower than PostgreSQL.
  SQLite allows only one writer per database at a time, which limits how many tasks Zendrite can process in parallel.

## Requirements

Zendrite runs on Linux, macOS, and Windows Server.
It should also run on BSD variants such as FreeBSD and OpenBSD.
Zendrite is untested on AIX, Solaris, Plan 9, and z/OS.

The CPU, RAM, and disk space Zendrite needs vary considerably.
The main factors are:

- The number of users on the server.
- The number of rooms the server has joined.
  Federated rooms typically use more resources than rooms with only local users.
- **The complexity of the joined rooms.**
  Rooms where many members come and go are typically much more complex.

Some tasks cost more than others, such as joining rooms over federation, running state resolution, or sending messages into large federated rooms with many remote users.
CPU and RAM spikes during these tasks can be considerably higher than idle usage, so plan enough headroom for them.

Zendrite needs at least 1GB RAM.
For a comfortable day-to-day deployment where several local users take part in federated rooms, assign 2-4 CPU cores and 8GB RAM.
Add more as your room count grows.

If PostgreSQL runs on the same machine, add headroom for its own CPU and RAM needs.
Too many heavy services on one machine can starve each other of resources, and the operating system may kill processes that use too much memory.

### Saving memory

To save RAM at the cost of more database CPU load, disable the cache in `zendrite.yaml`:

```yaml
global:
  cache:
    max_age: -1s
```

## Dependencies

Install the following dependencies before you install Zendrite.

### Go

Build Zendrite with Go 1.26 or later.
Older Go versions are unsupported.
Go 1.21 and later download newer Go versions automatically as needed.
If you install Go with a package manager, run `go version` to check the version before you start.

### PostgreSQL

If you use PostgreSQL, install PostgreSQL 17 or later.

### NATS Server

Zendrite includes a built-in [NATS Server](https://github.com/nats-io/nats-server), so you don't need to install one.

### Reverse proxy

Most deployments use a reverse proxy such as [Caddy](https://caddyserver.com), [NGINX](https://www.nginx.com), or [HAProxy](http://www.haproxy.org).
This documentation doesn't cover proxy configuration.
You may need to disable or raise the proxy's default request timeouts and POST payload limits.
Matrix clients expect to control when, or if, a request times out, and homeservers expect to control media file size limits.
Caddy has no timeouts or payload limits by default.

### Windows

To build Zendrite on Windows, you need `gcc` in the path.
The best way to get it is to install and build Zendrite under [MinGW-w64](https://www.mingw-w64.org/).
