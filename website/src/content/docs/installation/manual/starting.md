---
title: Starting Zendrite
description: Running your Zendrite server
---

After you finish the preparation and installation steps, start Zendrite by running the `zendrite` binary:

```bash
./zendrite -config /path/to/zendrite.yaml
```

By default, Zendrite listens for HTTP on port 8008.
To change the addresses or ports Zendrite listens on, use the `-http-bind-address` and `-https-bind-address` command line arguments:

```bash
./zendrite -config /path/to/zendrite.yaml \
    -http-bind-address 1.2.3.4:12345 \
    -https-bind-address 1.2.3.4:54321
```

To list the command line options:

```bash
./zendrite -help
```

## Systemd example

To run Zendrite as a systemd service, install a unit like this at `/etc/systemd/system/zendrite.service`:

```ini
[Unit]
Description=Zendrite homeserver

Wants=network-online.target postgresql.service caddy.service
After=network-online.target postgresql.service

[Service]
Type=exec
User=zendrite
WorkingDirectory=/home/zendrite
# Joining big rooms can require more than 1024 file descriptors
LimitNOFILE=65535
# By default it loads zendrite.yaml from the working directory and listens on port 8008 on all interfaces
ExecStart=/home/zendrite/bin/zendrite -http-bind-address 127.0.0.1:8008
Restart=always

# --- Hardening: filesystem ---
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=/home/zendrite
PrivateTmp=true
PrivateDevices=true
ProtectProc=invisible
ProcSubset=pid

# --- Hardening: kernel & host ---
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectKernelLogs=true
ProtectControlGroups=true
ProtectClock=true
ProtectHostname=true
LockPersonality=true

# --- Hardening: privileges ---
NoNewPrivileges=true
CapabilityBoundingSet=
AmbientCapabilities=
RestrictSUIDSGID=true
RemoveIPC=true
UMask=0077

# --- Hardening: network & namespaces ---
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
RestrictNamespaces=true
RestrictRealtime=true
SystemCallArchitectures=native

[Install]
WantedBy=multi-user.target
```

Notes on the hardening block:

- `ProtectHome=read-only` plus `ReadWritePaths=/home/zendrite` is the trade-off for keeping state under `/home/zendrite`.
  Do **not** "tighten" this to `ProtectHome=true` while Zendrite keeps its state in `/home/zendrite`.
  `true` makes the directory inaccessible (not only read-only) and overrides the `ReadWritePaths=` exemption, which locks Zendrite out of its own state.
  Debian-style packaging instead uses `/var/lib/zendrite` as the user's home _and_ `WorkingDirectory=`.
  In that layout, you can drop `ReadWritePaths=` and use `ProtectHome=true` safely, because `ProtectHome` covers only `/home`, `/root`, and `/run/user`.
- The unit leaves `SystemCallFilter` unset on purpose.
  `@system-service` is broad, but failures surface as silent `EPERM` or `SIGSYS`, which are hard to debug.
  Add it if you want it and have tested your build.
- `CapabilityBoundingSet=` (empty) is safe because the example binds to `127.0.0.1:8008`.
  To bind to a port below 1024, set `CAP_NET_BIND_SERVICE` on both `CapabilityBoundingSet` and `AmbientCapabilities`.
- `RestrictAddressFamilies` includes `AF_UNIX` for Postgres-over-socket setups and `AF_INET6` for outgoing IPv6 federation.
- The unit omits `MemoryDenyWriteExecute=true` on purpose, because Go's runtime and cgo can need W+X pages in some configurations.
  Test before you enable it.

Score the resulting unit with:

```bash
systemd-analyze security zendrite.service
```
