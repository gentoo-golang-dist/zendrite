---
title: Complement
description: Running the Complement Matrix conformance tests against Zendrite, and how CI gates on them
---

[Complement](https://github.com/matrix-org/complement) is the Matrix black-box test suite.
It starts Zendrite in Docker containers and tests the Client-Server and Server-Server APIs against them, playing remote servers itself where a test needs one.

## In CI

The `Complement` workflow (`.forgejo/workflows/complement.yaml`) runs `build/scripts/complement.sh` on SQLite and PostgreSQL.

- Pull requests that touch Go code, and pushes to `main`, run the tests in `test/complement/passing.txt` with reused homeservers (`COMPLEMENT_ENABLE_DIRTY_RUNS`).
- Manual runs (`workflow_dispatch`) run every test with a fresh homeserver per test.

A listed test that fails, or does not run, fails the job.
Failed listed tests are retried once with fresh homeservers first; a test that passes on retry is reported as flaky and does not fail the job.
Tests that are not listed never fail the job.
A manual run prints the unlisted tests that passed; add those to `passing.txt` once they pass on both backends.
Tests in `test/complement/flaky.txt` change result between runs on unchanged code, so they are neither gated nor suggested.

The job uploads `results.json` (the `go test -json` output), `retry.json` and `summary.txt` as an artifact.

The Complement commit is pinned in the workflow, and Renovate moves it to the tip of Complement's `main` branch.
A Complement update that breaks a listed test fails the Renovate pull request.

## Running locally

`build/scripts/complement.sh` needs Docker with buildx and Go.

```sh
# the listed tests, as on a pull request
COMPLEMENT_REF=<commit> build/scripts/complement.sh sqlite pr
# every test, as a manual workflow run does
COMPLEMENT_REF=<commit> build/scripts/complement.sh postgres full
```

Take the commit from `COMPLEMENT_REF` in the workflow.
Results land in `.complement/<backend>/`.

To run single tests while debugging, build a homeserver image and run Complement from a checkout:

```sh
docker build -t complement-zendrite -f build/scripts/Complement.Dockerfile .
# in a Complement checkout
COMPLEMENT_BASE_IMAGE=complement-zendrite go test -count=1 -v -run '^TestLogin$' ./tests/csapi
```

Use `build/scripts/ComplementPostgres.Dockerfile` for PostgreSQL.
Set `COMPLEMENT_ALWAYS_PRINT_SERVER_LOGS=1` to see Zendrite's logs for passing tests too.
Run one Complement process per Docker daemon at a time: two in parallel slow the homeservers down until they time out.

## How it runs

Complement reaches the homeservers on ports they publish on `127.0.0.1`, and they reach Complement through `host.docker.internal`.
Both only work from the Docker daemon's own network namespace, which a CI job container is not in.
`complement.sh` therefore runs Complement in a container on the daemon's host network, with the daemon's socket mounted (`build/scripts/ComplementRunner.Dockerfile`).

Complement names its containers, networks and images after the test package, and removes all of them before and after each package.
The runner image patches Complement to append `COMPLEMENT_NAMESPACE_SUFFIX` to those names, and `complement.sh` sets a suffix per run, so concurrent jobs on one daemon do not remove each other's homeservers.
