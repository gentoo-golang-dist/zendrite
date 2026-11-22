#!/usr/bin/env bash
# Run a selection of Sytest (https://github.com/matrix-org/sytest) test files
# against Zendrite, and fail if a test that is not in sytest-blacklist fails.
#
# Usage: build/scripts/sytest.sh <sqlite|postgres> [test file...]
#
# Without test files it runs SYTEST_FILES below: the files whose every test
# passes on both backends, which guard the entries taken off the blacklist.
# Paths are relative to the Sytest checkout, such as
# tests/60app-services/02ghost.pl.
#
# Nothing is bind mounted, so it works when the Docker daemon does not share
# the client's filesystem, as in CI: Zendrite and Sytest are built into one
# image, and the results are copied out of the container afterwards.
#
# Requirements: docker with buildx.
#
# Environment:
#   SYTEST_REF        Sytest commit to test with (required)
#   SYTEST_ARTIFACTS  output directory (default .sytest/<backend>)

set -euo pipefail

SYTEST_FILES=(
  tests/41end-to-end-keys/06-device-lists.pl
  tests/50federation/44presence.pl
  tests/60app-services/02ghost.pl
  tests/60app-services/03passive.pl
)

BACKEND="${1:-}"
case "${BACKEND}" in
  sqlite) POSTGRES=0 ;;
  postgres) POSTGRES=1 ;;
  *)
    echo "usage: $0 <sqlite|postgres> [test file...]" >&2
    exit 2
    ;;
esac
shift
[[ $# -gt 0 ]] && SYTEST_FILES=("$@")
: "${SYTEST_REF:?set SYTEST_REF to a Sytest commit}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

ARTIFACTS="${SYTEST_ARTIFACTS:-.sytest/${BACKEND}}"
RUN_ID="${BACKEND}$(date +%s)$$"
IMAGE="zendrite-sytest:${RUN_ID}"
CONTAINER="zendrite-sytest-${RUN_ID}"

log() { echo "[sytest] $*"; }

cleanup() {
  docker rm -f "${CONTAINER}" >/dev/null 2>&1 || true
  docker rmi "${IMAGE}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

rm -rf "${ARTIFACTS}"
mkdir -p "${ARTIFACTS}"

log "building ${IMAGE} (Sytest ${SYTEST_REF})"
SECONDS=0
docker buildx build --load -t "${IMAGE}" --build-arg "SYTEST_REF=${SYTEST_REF}" \
  -f build/scripts/Sytest.Dockerfile .
log "image built in ${SECONDS}s"

SECONDS=0
status=0
docker run --name "${CONTAINER}" -e "POSTGRES=${POSTGRES}" "${IMAGE}" "${SYTEST_FILES[@]}" || status=$?
log "tests ran in ${SECONDS}s"
docker cp "${CONTAINER}:/logs/." "${ARTIFACTS}/"

tap="${ARTIFACTS}/results.tap"
if [[ ! -s "${tap}" ]]; then
  echo "[sytest] no results in ${tap}" >&2
  exit 1
fi
passed=$(grep -c '^ok ' "${tap}" || true)
failed=$(grep '^not ok ' "${tap}" | grep -vc '# TODO expected fail' || true)
log "${passed} passed, ${failed} failed"
grep '^not ok ' "${tap}" | grep -v '# TODO expected fail' || true

if [[ "${status}" -ne 0 || "${failed}" -ne 0 || "${passed}" -eq 0 ]]; then
  echo "[sytest] failed; see ${tap} and the homeserver logs next to it" >&2
  exit 1
fi
