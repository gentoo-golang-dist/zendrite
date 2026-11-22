---
title: Generating signing keys
description: Creating and managing Matrix signing keys for Zendrite
---

Generate a Matrix signing private key with the `generate-keys` utility:

```bash
./bin/generate-keys --private-key matrix_key.pem
```

If you built Zendrite with `go build -o bin/ ./cmd/...`, `generate-keys` is in the `bin` folder.
The generated `matrix_key.pem` file is your new signing key.
Every Matrix homeserver needs one to authenticate federation requests and events.

Next, [configure Zendrite](/installation/manual/configuration).

## Important warning

Treat this key as highly sensitive and private: **never share it with anyone**.
No one should ever ask you for this key for any reason, even to debug a problematic Zendrite server.

Back up this key safely.
You will likely need it if you reinstall Zendrite, or any other Matrix homeserver, on the same domain name.
If you lose this key, you may have trouble joining federated rooms.

## Old signing keys

You can reuse signing keys from a previous Matrix installation on the same domain name.
A key that was ever marked as expired is unusable.

Keys from a previous Zendrite installation work as-is.
Set that key file in the Zendrite configuration.

If you have server keys from an older Synapse instance, convert them to Zendrite's PEM format and configure them as `old_private_keys` in your config.

## Key format

Zendrite stores the server signing key in PEM format with this structure:

```text
-----BEGIN MATRIX PRIVATE KEY-----
Key-ID: ed25519:<Key ID>

<Base64 Encoded Key Data>
-----END MATRIX PRIVATE KEY-----
```

## Converting Synapse keys

If you have signing keys from a previous Synapse installation on the same domain name, configure them as `old_private_keys` in your Zendrite config file.
Synapse stores signing keys in this format:

```text
ed25519 <Key ID> <Base64 Encoded Key Data>
```

To convert the key to Zendrite's PEM format, use the following template.
Copy the Key ID exactly.
**Add a trailing equals sign to the Base64 Encoded Key Data** if the original key lacks one.
The key data must be padded to exactly 32 bytes.

```text
-----BEGIN MATRIX PRIVATE KEY-----
Key-ID: ed25519:<Key ID>

<Base64 Encoded Key Data>=
-----END MATRIX PRIVATE KEY-----
```
