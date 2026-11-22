#!/bin/bash
# Entry point of the image built from build/scripts/Sytest.Dockerfile: runs
# the given Sytest test files against Zendrite and writes /logs/results.tap.
#
# Set POSTGRES=1 to run Zendrite on PostgreSQL; otherwise it uses SQLite.
# Exits non-zero when a test that is not in sytest-blacklist fails.

set -euo pipefail

cd /sytest
mkdir -p /work /logs

# Sytest reads database.yaml for both backends, so PostgreSQL always runs.
# The variables expand in the postgres user's shell, not this one.
# shellcheck disable=SC2016
su -c 'eatmydata /usr/lib/postgresql/*/bin/pg_ctl -w -D $PGDATA start' postgres >/dev/null
# shellcheck disable=SC2016
su -c 'for i in pg1 pg2 sytest_template; do psql -q -c "CREATE DATABASE $i;"; done' postgres
export PGUSER=postgres POSTGRES_DB_1=pg1 POSTGRES_DB_2=pg2
./scripts/prep_sytest_for_postgres.sh

status=0
./run-tests.pl -I Dendrite::Monolith -d /zbin -B /blacklist -O tap --all \
  --work-directory=/work --exclude-deprecated "$@" >/logs/results.tap || status=$?

# Keep the homeserver logs, not their databases.
for server in server-0 server-1; do
  if [[ -d "/work/${server}/dendrite-logs" ]]; then
    mkdir -p "/logs/${server}"
    cp -r "/work/${server}/dendrite-logs" "/logs/${server}/"
  fi
done

exit "${status}"
