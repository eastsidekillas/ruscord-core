#!/usr/bin/env sh
# Restores a backup made by scripts/backup.sh into the compose stack.
#
#   scripts/restore.sh <root>/<UTC time> [database]
#
# Without a database name it replaces the live "ruscord" database: stop the
# app first (docker compose ... stop app). With a name (e.g. ruscord_check) it
# restores into a new database next to the live one, to check a backup
# without touching anything; files are not uploaded then.
set -eu
cd "$(dirname "$0")/.."
COMPOSE="docker compose --env-file .env -f deploy/docker-compose.yml"
dump_dir="${1:?usage: scripts/restore.sh <backup dir> [database]}"
db="${2:-ruscord}"
root="$(dirname "$dump_dir")"

(cd "$dump_dir" && (sha256sum -c SHA256SUMS 2>/dev/null || shasum -a 256 -c SHA256SUMS))

if [ "$db" = "ruscord" ]; then
  $COMPOSE exec -T postgres pg_restore -U ruscord -d ruscord --clean --if-exists --no-owner < "$dump_dir/postgres.dump"
  root_abs="$(cd "$root" && pwd)"
  $COMPOSE --profile app run --rm --no-deps --user "$(id -u):$(id -g)" \
    -v "$root_abs/files:/backup:ro" app files-restore /backup
else
  $COMPOSE exec -T postgres dropdb -U ruscord --if-exists "$db"
  $COMPOSE exec -T postgres createdb -U ruscord "$db"
  $COMPOSE exec -T postgres pg_restore -U ruscord -d "$db" --no-owner < "$dump_dir/postgres.dump"
fi
echo "restored $dump_dir into database $db"
