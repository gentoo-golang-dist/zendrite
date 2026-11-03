#!/usr/bin/env bash
# Run Complement (https://github.com/matrix-org/complement) against Zendrite
# and gate the result on test/complement/passing.txt.
#
# Usage: build/scripts/complement.sh <sqlite|postgres> [pr|full]
#
#   pr    run the tests in passing.txt with reused homeservers
#         (COMPLEMENT_ENABLE_DIRTY_RUNS) and fail if one of them fails.
#   full  run every test with a fresh homeserver per test, fail if a listed
#         test fails, and list the unlisted tests that passed.
#
# Listed tests that fail are retried once with fresh homeservers; a test that
# passes on retry is reported as flaky and does not fail the run.
#
# Complement reaches the homeservers on ports they publish on 127.0.0.1, and
# they reach Complement through host.docker.internal. Both only work from the
# Docker daemon's own network namespace, which a CI job container is not in,
# so Complement runs in a container on the daemon's host network with the
# daemon's socket mounted.
#
# Requirements: docker with buildx, go.
#
# Environment:
#   COMPLEMENT_REF            Complement commit to test with (required)
#   COMPLEMENT_ARTIFACTS      output directory (default .complement/<backend>)
#   COMPLEMENT_DOCKER_SOCKET  daemon socket path as the daemon sees it
#                             (default /var/run/docker.sock)

set -euo pipefail

BACKEND="${1:-}"
MODE="${2:-pr}"
case "${BACKEND}" in
  sqlite) DOCKERFILE=build/scripts/Complement.Dockerfile ;;
  postgres) DOCKERFILE=build/scripts/ComplementPostgres.Dockerfile ;;
  *)
    echo "usage: $0 <sqlite|postgres> [pr|full]" >&2
    exit 2
    ;;
esac
case "${MODE}" in
  pr | full) ;;
  *)
    echo "usage: $0 <sqlite|postgres> [pr|full]" >&2
    exit 2
    ;;
esac
: "${COMPLEMENT_REF:?set COMPLEMENT_REF to a Complement commit}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

ARTIFACTS="${COMPLEMENT_ARTIFACTS:-.complement/${BACKEND}}"
SOCKET="${COMPLEMENT_DOCKER_SOCKET:-/var/run/docker.sock}"
LIST=test/complement/passing.txt
FLAKY=test/complement/flaky.txt
# Docker names allow [a-zA-Z0-9_.-]; keep the suffix short and plain.
RUN_ID="${BACKEND}$(date +%s)$$"
SUFFIX="_${RUN_ID}"
HS_IMAGE="zendrite-complement-${BACKEND}:${RUN_ID}"
RUNNER_IMAGE="zendrite-complement-runner:${COMPLEMENT_REF:0:12}"

log() { echo "[complement] $*"; }
gate() { go run ./test/complement/gate "$@"; }

# Remove what this run left behind: Complement cleans up after each package,
# but not when the job is cancelled half way.
cleanup() {
  docker ps -a --filter label=complement_pkg --format "${OWN_FORMAT}" | own | xargs -r docker rm -f >/dev/null 2>&1 || true
  docker network ls --filter label=complement_pkg --format "${OWN_FORMAT}" | own | xargs -r docker network rm >/dev/null 2>&1 || true
  docker images --filter label=complement_pkg --format "${OWN_FORMAT}" | own | xargs -r docker rmi -f >/dev/null 2>&1 || true
  docker rmi "${HS_IMAGE}" >/dev/null 2>&1 || true
}
OWN_FORMAT='{{.ID}} {{.Label "complement_pkg"}}'
# own reads "<id> <namespace>" lines and prints the ids of this run's objects.
own() { awk -v s="${SUFFIX}" 'substr($2, length($2) - length(s) + 1) == s { print $1 }'; }
trap cleanup EXIT

rm -rf "${ARTIFACTS}"
mkdir -p "${ARTIFACTS}"

log "building ${HS_IMAGE} and ${RUNNER_IMAGE} (Complement ${COMPLEMENT_REF})"
SECONDS=0
docker buildx build --load -t "${HS_IMAGE}" -f "${DOCKERFILE}" .
docker buildx build --load -t "${RUNNER_IMAGE}" --build-arg "COMPLEMENT_REF=${COMPLEMENT_REF}" \
  -f build/scripts/ComplementRunner.Dockerfile build/scripts
log "images built in ${SECONDS}s"

# run_package <package> <-run expression> <output> <dirty>
run_package() {
  local pkg="$1" expr="$2" out="$3" dirty="$4"
  local start=${SECONDS}
  # go test exits non-zero when a test fails; the gate decides.
  docker run --rm --network host \
    -v "${SOCKET}:/var/run/docker.sock" \
    -e COMPLEMENT_BASE_IMAGE="${HS_IMAGE}" \
    -e COMPLEMENT_NAMESPACE_SUFFIX="${SUFFIX}" \
    -e COMPLEMENT_ENABLE_DIRTY_RUNS="${dirty}" \
    -e COMPLEMENT_SPAWN_HS_TIMEOUT_SECS=60 \
    "${RUNNER_IMAGE}" \
    go test -p 1 -count=1 -timeout 30m -json -run "${expr}" "./${pkg}" >>"${out}" || true
  log "${pkg}: $((SECONDS - start))s"
}

SECONDS=0
if [[ "${MODE}" == pr ]]; then
  while read -r pkg; do
    run_package "${pkg}" "$(gate regex -list "${LIST}" -pkg "${pkg}")" "${ARTIFACTS}/results.json" 1
  done < <(gate packages -list "${LIST}")
else
  while read -r pkg; do
    run_package "${pkg}" "" "${ARTIFACTS}/results.json" 0
  done < <(docker run --rm "${RUNNER_IMAGE}" go list ./tests/... | sed 's%^github.com/matrix-org/complement/%%')
fi
log "tests ran in ${SECONDS}s"

touch "${ARTIFACTS}/retry.json"
while read -r pkg expr; do
  log "retrying in ${pkg}: ${expr}"
  run_package "${pkg}" "${expr}" "${ARTIFACTS}/retry.json" 0
done < <(gate failed -list "${LIST}" "${ARTIFACTS}/results.json")

full_flag=()
[[ "${MODE}" == full ]] && full_flag=(-full)
gate check -list "${LIST}" -flaky "${FLAKY}" -retry "${ARTIFACTS}/retry.json" "${full_flag[@]}" \
  "${ARTIFACTS}/results.json" | tee "${ARTIFACTS}/summary.txt"
