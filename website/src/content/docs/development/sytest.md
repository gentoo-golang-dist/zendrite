---
title: Sytest
description: Run Sytest, the older Matrix integration test suite, against Zendrite, and see which part of it CI runs.
---

[Sytest](https://github.com/matrix-org/sytest) is the older Matrix integration test suite, written in Perl.
It starts two Zendrite servers and tests them through the Client-Server, Server-Server, and Application Service APIs.

## Run locally

`build/scripts/sytest.sh` needs Docker with buildx.

```sh
# the test files CI runs
SYTEST_REF=<commit> build/scripts/sytest.sh sqlite
# other test files, paths relative to the Sytest checkout
SYTEST_REF=<commit> build/scripts/sytest.sh postgres tests/50federation/36state.pl
```

Take the commit from `SYTEST_REF` in the workflow.
The script writes `results.tap` and the homeserver logs to `.sytest/<backend>/`.
A test that fails and isn't in `sytest-blacklist` fails the run.

## In CI

The `Sytest` workflow (`.forgejo/workflows/sytest.yaml`) runs `build/scripts/sytest.sh` on SQLite and PostgreSQL for pull requests that touch Go code or the Sytest files, and for pushes to `main`.
It runs only the test files listed in `SYTEST_FILES` in the script, whose tests all pass, and takes a few minutes.
The job uploads `results.tap` and the homeserver logs as an artifact.
Renovate moves `SYTEST_REF` to the tip of Sytest's `develop` branch in a pull request, so the workflow checks each Sytest change before Zendrite takes it.

The whole suite isn't in CI.
Some test files still have failing tests that `sytest-blacklist` doesn't list, so a full run would fail.
Before you add a test file to `SYTEST_FILES`, run it at least 10 times on each backend.

## How it runs

The `matrixdotorg/sytest-dendrite` image builds Dendrite from source with an older Go than Zendrite needs.
So `build/scripts/Sytest.Dockerfile` builds Zendrite's binaries itself, names them the way Sytest's Dendrite module expects, and adds them to that image with the pinned Sytest commit and `sytest-blacklist`.
Nothing is bind mounted, so the script also works when the Docker daemon doesn't share the job's filesystem, and it copies the results out of the container afterwards.

Sytest creates its rooms in room version 12 by default.
Its redaction algorithm for room versions 11 and later still keeps `origin` and `prev_state`.
Zendrite's events include both fields, so for some federation tests Sytest computes other event IDs than Zendrite does.
Add `--room-version 10` after the test files to check whether a failure comes from this.
