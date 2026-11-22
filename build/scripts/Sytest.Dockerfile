#syntax=docker/dockerfile:1.29

# Sytest (https://github.com/matrix-org/sytest) with Zendrite built in.
# The upstream image builds ./cmd/dendrite with Go 1.22, which cannot build
# Zendrite, so the binaries are built here and named the way Sytest's Dendrite
# module expects. See build/scripts/sytest.sh.

FROM golang:1.27-trixie AS build
WORKDIR /build
# Static binaries: the Sytest image runs an older Debian than this one.
RUN --mount=target=. \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    mkdir /zbin && \
    CGO_ENABLED=0 go build -tags nodynamic -trimpath -o /zbin/ ./cmd/generate-config ./cmd/generate-keys ./cmd/zendrite && \
    ln -s zendrite /zbin/dendrite

FROM docker.io/matrixdotorg/sytest-dendrite:latest@sha256:e58b549aa1e7270c2c8de5e656f75742e9cba46465046b8f2b0e59d37c10734d
ARG SYTEST_REF
RUN test -n "${SYTEST_REF}" && \
    wget -qO /tmp/sytest.tar.gz "https://github.com/matrix-org/sytest/archive/${SYTEST_REF}.tar.gz" && \
    mkdir /sytest && tar -C /sytest --strip-components=1 -xzf /tmp/sytest.tar.gz && \
    rm /tmp/sytest.tar.gz
COPY --from=build /zbin /zbin
COPY sytest-blacklist /blacklist
COPY build/scripts/sytest-run.sh /sytest-run.sh
ENTRYPOINT ["/bin/bash", "/sytest-run.sh"]
