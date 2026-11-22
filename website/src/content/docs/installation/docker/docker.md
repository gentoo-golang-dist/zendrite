---
title: Docker Setup
description: Installing and running Zendrite with Docker
---

Zendrite provides an example Docker Compose file in `build/docker/docker-compose.yaml`.
Before it starts, generate a private key and a config as described below.
The compose file includes only PostgreSQL as a dependency, so you also need to configure a [reverse proxy](/installation/planning#reverse-proxy).

Docker images are available from:

- [Docker Hub](https://hub.docker.com/r/pats22/zendrite): `pats22/zendrite`
- [Codefloe Registry](https://codefloe.com/pat-s/-/packages/container/zendrite): `codefloe.com/pat-s/zendrite`

## Preparations

### Generate a private key

Generate a private key, which Zendrite uses to sign events.
This command creates it in `./config`:

```bash
mkdir -p ./config
docker run --rm --entrypoint="/usr/bin/generate-keys" \
  -v $(pwd)/config:/mnt \
  pats22/zendrite:latest \
  -private-key /mnt/matrix_key.pem
```

Run this command only **once**, because running it again overwrites the key.

### Generate a config

Generate a config that uses the paths from the example compose file.
Set `server` to your domain, and set `db` to match the `services.postgres.environment` values in your compose file:

```bash
mkdir -p ./config
docker run --rm --entrypoint="/bin/sh" \
  -v $(pwd)/config:/mnt \
  pats22/zendrite:latest \
  -c "/usr/bin/generate-config \
    -dir /var/lib/zendrite/ \
    -db postgres://zendrite:itsasecret@postgres/zendrite?sslmode=disable \
    -server YourDomainHere > /mnt/zendrite.yaml"
```

Then edit `config/zendrite.yaml` to suit your installation.
See [Configuring Zendrite](/installation/manual/configuration) for the options.

## Starting Zendrite

Start Zendrite:

```bash
docker compose up
```
