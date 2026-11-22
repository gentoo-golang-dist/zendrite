---
title: Creating user accounts
description: Create user accounts on your Zendrite server
---

Create accounts with the `create-account` tool or the shared secret registration endpoint.

## From the command line

[Building](/installation/manual/build) Zendrite puts the `create-account` tool in the `bin` folder.
The tool reads `zendrite.yaml` to connect to a **running** Zendrite instance.
It needs [shared secret registration](#using-shared-secret-registration) enabled.

Create a **normal account**:

```bash
./bin/create-account -config /path/to/zendrite.yaml -username USERNAME
```

The tool prompts you for the new account's password.

Add the `-admin` flag to create an **admin account**:

```bash
./bin/create-account -config /path/to/zendrite.yaml -username USERNAME -admin
```

`create-account` connects to `http://localhost:8008` by default.
Set a different address with the `-url` flag:

```bash
./bin/create-account -config /path/to/zendrite.yaml -username USERNAME -url https://localhost:8448
```

In **Docker**, find the `CONTAINERNAME` with `docker ps`, then run the tool inside the container:

```bash
docker exec -it CONTAINERNAME /usr/bin/create-account -config /path/to/zendrite.yaml -username USERNAME
```

```bash
docker exec -it CONTAINERNAME /usr/bin/create-account -config /path/to/zendrite.yaml -username USERNAME -admin
```

## Using shared secret registration

Zendrite supports the Synapse-compatible shared secret registration endpoint.

Enable it by setting a secret in the `registration_shared_secret` field of the `client_api` section in `zendrite.yaml`:

```yaml
client_api:
  # ...
  registration_shared_secret: ""
```

Then call the `/_synapse/admin/v1/register` endpoint as described in the [Synapse documentation](https://element-hq.github.io/synapse/latest/admin_api/register_api.html).

The endpoint stays disabled until you set a secret.
Remove the secret from the config to disable it again.
