#syntax=docker/dockerfile:1.28

# Complement's test suite at a pinned commit, compiled, for
# build/scripts/complement.sh. The homeservers under test come from
# Complement.Dockerfile or ComplementPostgres.Dockerfile.
FROM golang:1.27-trixie

ARG COMPLEMENT_REF
WORKDIR /complement
RUN test -n "${COMPLEMENT_REF}" && \
    git init -q . && \
    git remote add origin https://github.com/matrix-org/complement && \
    git fetch -q --depth 1 origin "${COMPLEMENT_REF}" && \
    git checkout -q FETCH_HEAD

# Complement names its containers, networks and images after the test package
# and, before and after a package, removes every one carrying that name. Two
# runs on one Docker daemon would remove each other's homeservers, so let
# complement.sh add a per-run suffix. Fail if a new Complement version no
# longer has the line this patches.
RUN sed -i 's/^\tcfg.PackageNamespace = pkgNamespace$/\tcfg.PackageNamespace = pkgNamespace + os.Getenv("COMPLEMENT_NAMESPACE_SUFFIX")/' config/config.go && \
    grep -q 'os.Getenv("COMPLEMENT_NAMESPACE_SUFFIX")' config/config.go

# Compile every test package without running it, so runs start from a warm
# build cache.
RUN go test -count=1 -run '^$' -exec /bin/true ./tests/...
