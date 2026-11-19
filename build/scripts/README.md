# Dev Scripts

These scripts help with Zendrite development.

Run `./build/scripts/build-test-lint.sh` to build all commands, lint the repository, and run the unit tests with the race detector.
The script requires Go and `prek` on `PATH` and can be invoked from any working directory.
Tests use the `goolm` tag, matching the `just` recipes, so the end-to-end encryption tests do not need the C `libolm` library.
Builds do not need it, since no shipped binary depends on Olm.

To run linting alone, use `just lint` or `prek run golangci-lint --all-files` from the repository root.
`prek` manages the golangci-lint version pinned in `.pre-commit-config.yaml`, using the checked-in v2 configuration and its `test` and `goolm` build tags.
The obsolete `find-lint.sh` installer and its `fast` argument have been removed.
Linting does not use `--fix` or back up and overwrite module files.
Set `GOGC` to control garbage collection during linting if needed.

Run `build/scripts/complement.sh <sqlite|postgres> [pr|full]` to run the Complement conformance tests as CI does; see the Complement page of the development docs.
