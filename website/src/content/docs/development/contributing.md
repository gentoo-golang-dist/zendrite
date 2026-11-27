---
title: Contributing
description: Set up a development environment and follow the conventions for Zendrite changes.
---

Everyone is welcome to contribute to Zendrite.

## Set up a development environment

Build an instance of Zendrite as described in [Installation](/installation/planning).
You need one to test your changes.

For local development, Zendrite starts its built-in NATS server with JetStream when `global.jetstream.addresses` is empty.
Set `global.jetstream.storage_path` to a persistent, writable directory as described in [JetStream configuration](/installation/manual/configuration/#jetstream-configuration).
You don't need to install a separate messaging service for this setup.

## Pre-commit hooks

The project runs [pre-commit](https://pre-commit.com/) checks before each commit.
Install the hooks:

```bash
pre-commit install
```

The hooks run on `git commit` and include:

- **golangci-lint:** Go linting and formatting (see below).
- **markdownlint:** Markdown style.
- **prettier:** formatting for non-Go files (YAML, JS, CSS, and others).
- **yamllint:** YAML validation.
- **hadolint:** Dockerfile linting.
- **editorconfig-checker:** consistent editor settings.
- **commitlint:** Conventional Commit message validation (see below).

`commitlint` runs at the `commit-msg` stage.
`pre-commit install` installs that stage too, because the config sets `default_install_hook_types`.

## Commit and pull request titles

Commit messages and pull request titles must follow [Conventional Commits](https://www.conventionalcommits.org/) and **must include a scope**:

```text
<type>(<scope>): <subject>
```

The scope names the affected package, so changes group cleanly in the changelog, for example `fix(clientapi/routing): time out federated join requests`.
Nested scopes such as `clientapi/routing` are allowed.
Use the package closest to the change.

Allowed types are `build`, `chore`, `ci`, `docs`, `enh`, `feat`, `fix`, `perf`, `refactor`, `revert`, `style`, and `test`.
`WIP` is also allowed on local commits, but never on a mergeable pull request title.
Pull requests are squash-merged, so the title becomes the changelog entry.

The `commitlint` pre-commit hook enforces both rules locally.
In CI, the `Commitlint` workflow validates the pull request title.

## Code style

`golangci-lint` formats Go code by running `gofmt`, `gofumpt`, and `gci` (import ordering).
You don't need to run these by hand, because pre-commit applies them on each commit.

Import ordering follows the `gci` convention configured in `.golangci.yaml`:

1. Standard library
2. Third-party packages
3. `codefloe.com/pat-s/zendrite` (local packages)

## Comments

Write comments that explain _why_ your code does what it does.
Comment any statement whose purpose isn't obvious.

Use these tags so others can search for them:

- `// TODO:` marks code that likely needs a future review, rewrite, or refactor.
- `// FIXME:` marks a known outstanding bug.
- `// NOTSPEC:` marks behavior that deliberately differs from the [Matrix Specification](https://spec.matrix.org/), with a description of _why_.

## Linting

`.golangci.yaml` configures [golangci-lint](https://github.com/golangci/golangci-lint).
It runs as a pre-commit hook, and you can also run it by hand:

```bash
prek run golangci-lint --all-files
```

This command and `just lint`, which invokes the same hook, need `prek` on `PATH`.
The hook uses the version pinned in `.pre-commit-config.yaml` and the `test` and `goolm` build tags configured in `.golangci.yaml`.

If you're certain a linter warning is spurious, silence it with a `// nolint:` comment on the relevant line or method.
Avoid this where you can.

## Unit tests

Run the unit tests:

```bash
go test -tags goolm -race ./...
```

The `goolm` tag selects the pure-Go Olm implementation for the end-to-end encryption tests.
Without it, those tests need the C `libolm` library.
The command runs both the SQLite and PostgreSQL tests.
The PostgreSQL tests need either a local PostgreSQL install (they use `createdb`) or a remote or containerized PostgreSQL instance.

To connect to a remote PostgreSQL, set these environment variables:

```bash
POSTGRES_USER=postgres
POSTGRES_PASSWORD=yourPostgresPassword
POSTGRES_HOST=localhost
POSTGRES_DB=postgres # the superuser database to use
```

Include tests with your changes.
A test proves the code works as intended and catches future regressions early.

Tests use the standard [Go testing package](https://gobyexample.com/testing) plus helper functions in Zendrite's own `test` package.

## Changing shared state

During a [rolling upgrade](/administration/high-availability/#rolling-upgrades), an HA deployment runs an old and a new build against the same database and streams.
A change to that shared state needs a new HA upgrade level.
`TestContractSurface` in `setup/ha` creates every table in a fresh PostgreSQL database.
It then compares the schema, the recorded migrations, and the JetStream stream definitions with `setup/ha/testdata/surface.txt`.
When it fails, raise `Level` in `setup/ha/contract.go` and keep `UpgradeFrom` and `RollbackTo` unchanged.

Every release must stay a rolling upgrade from, and a rolling rollback to, every earlier release of the same protocol.
Deployments update unattended and may skip releases, so a level that raises `UpgradeFrom` or `RollbackTo` would turn some of those updates into an offline upgrade.
`TestLevelsKeepRollingUpgrades` refuses such a level.

Make the change backward compatible instead, such as a new table, a nullable column, or a new optional message field.
Split a change that older builds can't work with over several releases:

1. Add the new structure, and read and write both.
2. Stop using the old structure.
3. Keep the old structure, or leave it unused, rather than dropping it.

A change that older builds can never work with, such as a changed stream, starts a new protocol.
Raise `Protocol` and set `UpgradeFrom` and `RollbackTo` to the new level.
Mark the commit as a breaking change, so the release is a new major version and deployments that only automate minor and patch updates wait for an operator.

Then record the level with `go test ./setup/ha -run TestContractSurface -update`.
This adds the level to `setup/ha/testdata/levels.json`, the published upgrade matrix.
The test can't see message formats.
A change to a JetStream message that an older build can't decode needs the same treatment, even though no test fails.

## Continuous integration

When you open a pull request, Forgejo Actions runs CI jobs to check that the code builds and works.
CI runs `golangci-lint`, then runs the unit tests split by package group (syncapi, clientapi, roomserver, userapi, federationapi, and other) on both SQLite and PostgreSQL.

You can follow CI progress at the bottom of the pull request page, or in the repository's Actions tab.

A pull request needs all CI jobs to pass before it's accepted.
If a test itself is wrong, open a pull request that fixes the test.

### Run checks locally

Run the checks locally before you push, and fix any errors first.
This saves waiting on CI and frees CI capacity for other pull requests.

The pre-commit hooks run `golangci-lint` and other checks on each commit.
To run them against all files:

```bash
pre-commit run --all-files
```

To run the full test suite:

```bash
go test -tags goolm -race ./...
```

If both pass, the code should pass CI.
