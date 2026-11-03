# Docker images

Docker images for Zendrite are available from:

- [Docker Hub](https://hub.docker.com/r/pats22/zendrite): `docker.io/pats22/zendrite`
- [Codefloe Registry](https://codefloe.com/pat-s/-/packages/container/zendrite): `codefloe.com/pat-s/zendrite`

## Dockerfile

The `Dockerfile` is a multistage file which can build Zendrite.
From the root of the Zendrite repository, run:

```bash
docker build -t pats22/zendrite:latest .
```

## Compose file

The `docker-compose.yaml` file runs a Zendrite deployment with Postgres.

## Configuration

The compose file refers to the `./config` volume as where the runtime config should come from.
The mounted folder must contain:

- `zendrite.yaml` configuration file (based on the [`zendrite-sample.yaml`](https://codefloe.com/pat-s/zendrite/src/branch/main/zendrite-sample.yaml))
- `matrix_key.pem` server key, as generated using `generate-keys`

To generate a signing key:

```bash
mkdir -p ./config
docker run --rm --entrypoint="/usr/bin/generate-keys" \
  -v $(pwd)/config:/mnt \
  pats22/zendrite:latest \
  -private-key /mnt/matrix_key.pem
```

The key file will now exist in `./config` and can be mounted into place.

## Starting Zendrite

Create your config based on the [`zendrite-sample.yaml`](https://codefloe.com/pat-s/zendrite/src/branch/main/zendrite-sample.yaml) sample configuration file, or follow the [Docker setup guide](https://zendrite.pat-s.me/installation/docker/docker/), which generates one with the paths this compose file uses.

Then start the deployment:

```bash
docker compose up
```
