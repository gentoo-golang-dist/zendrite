---
title: Starting Zendrite
description: Running your Zendrite server
---

Once you have completed all preparation and installation steps, you can start your Zendrite deployment by executing the `zendrite` binary:

```bash
./zendrite -config /path/to/zendrite.yaml
```

By default, Zendrite will listen HTTP on port 8008.
If you want to change the addresses or ports that Zendrite listens on, you can use the `-http-bind-address` and `-https-bind-address` command line arguments:

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

You could install a service something like this at `/etc/systemd/system/zendrite.service`:
```
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

[Install]
WantedBy=multi-user.target
```

