---
title: Building/Installing Zendrite
description: How to build and install Zendrite from source
---

Build the server and all utility commands from the root of the source repository:

```sh
go build -o bin/ ./cmd/...
```

Go writes the binaries to the `bin` subfolder.
Next, [prepare the database](/installation/manual/database).

## Installing Zendrite

To install the Zendrite binary into `$GOPATH/bin`, use `go install`:

```sh
go install ./cmd/zendrite
```

To write the binary to a custom path, use `go build`:

```sh
go build -o /usr/local/bin/ ./cmd/zendrite
```

## Build flags for packagers

Zendrite needs no special flags to build or perform well.
A release build should set only the version, which otherwise reports `dev`:

```sh
go build -trimpath \
  -ldflags "-X codefloe.com/pat-s/zendrite/internal.version=$VERSION" \
  -o bin/ ./cmd/...
```

`-trimpath` keeps builds reproducible and matches the official container image.
Distribution hardening flags such as `-buildmode=pie` or stripping with `-s -w` are a packaging choice and don't change Zendrite's behaviour.

Two optional build tags change which libraries the binary uses:

- `libsqlite3` links the system SQLite library instead of the bundled copy.
  It requires CGO and the SQLite development headers.
- `nodynamic` makes WebP thumbnailing always use the built-in decoder.
  Without it, Zendrite loads `libwebp.so` at runtime when present and falls back to the built-in decoder otherwise.
  Set it for static builds (`CGO_ENABLED=0`), as the official container image does.

The `goolm` tag matters only for the test suite.
It selects the pure-Go Olm implementation that the end-to-end encryption tests use.
Pass it to `go test` (for example `go test -tags goolm ./...`) unless the C `libolm` library is installed.
A `-tags` flag on the command line replaces any tags set in `GOFLAGS`, so list all tags in one place.
