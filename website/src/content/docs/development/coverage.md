---
title: Coverage
description: Run the unit tests with code coverage.
---

## Run unit tests with coverage

Run the unit tests with coverage and print a per-function summary:

```bash
go test -covermode=atomic -coverpkg=./... -coverprofile=integrationcover.log $(go list ./... | grep -v '/cmd/')
go tool cover -func=integrationcover.log
```

The first command writes the profile to `integrationcover.log`.

To view coverage as an HTML report:

```bash
go tool cover -html=integrationcover.log -o coverage.html
```
