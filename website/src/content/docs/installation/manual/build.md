---
title: Building/Installing Zendrite
description: How to build and install Zendrite from source
---

Zendrite has numerous utility commands in addition to the actual server binaries.
Build them all from the root of the source repo with:

```sh
go build -o bin/ ./cmd/...
```

The resulting binaries will be placed in the `bin` subfolder.

## Installing Zendrite

You can install the Zendrite binary into `$GOPATH/bin` by using `go install`:

```sh
go install ./cmd/zendrite
```

Alternatively, you can specify a custom path for the binary to be written to using `go build`:

```sh
go build -o /usr/local/bin/ ./cmd/zendrite
```

## Build flags for packagers

Zendrite needs no special flags to build or perform well.
The one flag a release build should set is the version, which otherwise reports `dev`:

```sh
go build -trimpath \
  -ldflags "-X codefloe.com/pat-s/zendrite/internal.version=$VERSION" \
  -o bin/ ./cmd/...
```

`-trimpath` keeps builds reproducible and matches the official container image.
Distribution hardening flags such as `-buildmode=pie` or stripping with `-s -w` are a packaging choice and do not change Zendrite's behaviour.

Two optional build tags change which libraries the binary uses:

- `libsqlite3` links the system SQLite library instead of the bundled copy. It requires CGO and the SQLite development headers.
- `nodynamic` makes WebP thumbnailing always use the built-in decoder. Without it, Zendrite loads `libwebp.so` at runtime when present and falls back to the built-in decoder otherwise. Static builds (`CGO_ENABLED=0`) should set it, as the official container image does.

The `goolm` tag only matters for the test suite, where it selects the pure-Go Olm implementation used by the end-to-end encryption tests.
Pass it to `go test` (for example `go test -tags goolm ./...`) unless the C `libolm` library is installed.
Note that a `-tags` flag on the command line replaces any tags set in `GOFLAGS`, so list all tags in one place.
