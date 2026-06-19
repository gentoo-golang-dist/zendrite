#!/bin/sh
set -eu

backend=${1:-}
case "$backend" in
  postgres|sqlite) ;;
  *)
    echo "usage: $0 postgres|sqlite" >&2
    exit 2
    ;;
esac

root=$(CDPATH='' cd -- "$(dirname "$0")/../.." && pwd)
tmp=$(mktemp -d)
network="zendrite-migration-$$"
data_volume="zendrite-migration-data-$$"
postgres_volume="zendrite-migration-postgres-$$"
dendrite_container="zendrite-migration-dendrite-$$"
zendrite_container="zendrite-migration-zendrite-$$"
postgres_container="zendrite-migration-postgres-$$"

cleanup() {
  docker rm -f "$dendrite_container" "$zendrite_container" "$postgres_container" >/dev/null 2>&1 || true
  docker volume rm -f "$data_volume" "$postgres_volume" >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT INT TERM

docker network create "$network" >/dev/null
docker volume create "$data_volume" >/dev/null

database_url=file:/data/homeserver.db
if [ "$backend" = postgres ]; then
  docker volume create "$postgres_volume" >/dev/null
  docker run -d \
    --name "$postgres_container" \
    --network "$network" \
    -e POSTGRES_HOST_AUTH_METHOD=trust \
    -e POSTGRES_DB=zendrite \
    -v "$postgres_volume:/var/lib/postgresql/data" \
    postgres:17-trixie >/dev/null

  until docker exec "$postgres_container" pg_isready -U postgres -d zendrite >/dev/null 2>&1; do
    sleep 1
  done
  database_url="postgres://postgres@${postgres_container}:5432/zendrite?sslmode=disable"
fi

git clone --depth=1 https://github.com/element-hq/dendrite.git "$tmp/dendrite"
echo "Testing Dendrite HEAD $(git -C "$tmp/dendrite" rev-parse HEAD)"
rm -rf "$tmp/dendrite/.git"
cp "$root/build/scripts/DendriteMigration.Dockerfile" "$tmp/dendrite/Dockerfile.migration"
cp "$root/build/scripts/dendrite-migration-entrypoint.sh" "$tmp/dendrite/build/scripts/"

docker build \
  -f "$tmp/dendrite/Dockerfile.migration" \
  --build-arg BINARY=dendrite \
  -t zendrite-migration-dendrite:head \
  "$tmp/dendrite"

docker build \
  -f "$root/build/scripts/DendriteMigration.Dockerfile" \
  --build-arg BINARY=zendrite \
  -t zendrite-migration-zendrite:current \
  "$root"

docker build \
  -f "$root/build/scripts/DendriteMigrationTest.Dockerfile" \
  -t zendrite-migration-test:current \
  "$root"

start_homeserver() {
  name=$1
  image=$2
  docker run -d \
    --name "$name" \
    --network "$network" \
    -e SERVER_NAME=hs1 \
    -e DATABASE_URL="$database_url" \
    -v "$data_volume:/data" \
    "$image" >/dev/null

  attempts=0
  until docker exec "$name" \
    curl --fail --silent "http://127.0.0.1:8008/_matrix/client/versions" >/dev/null; do
    attempts=$((attempts + 1))
    if [ "$attempts" -ge 120 ]; then
      docker logs "$name" >&2
      return 1
    fi
    sleep 1
  done
}

start_homeserver "$dendrite_container" zendrite-migration-dendrite:head
docker run --rm \
  --network "$network" \
  zendrite-migration-test:current \
  -url "http://$dendrite_container:8008" seed
docker rm -f "$dendrite_container" >/dev/null

start_homeserver "$zendrite_container" zendrite-migration-zendrite:current
docker run --rm \
  --network "$network" \
  zendrite-migration-test:current \
  -url "http://$zendrite_container:8008" verify
