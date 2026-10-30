#!/usr/bin/env sh
# Backs up the compose stack: a Postgres dump into <root>/<UTC time>/ and the
# stored files (S3) into <root>/files/. Files are content-addressed and never
# change, so <root>/files/ only grows by new files on each run.
#
#   scripts/backup.sh [root]       root defaults to backups/
#   RUSCORD_BACKUP_KEEP=7          dumps to keep (older ones are removed)
#
# Needs the stack up with the app image built (make up-app). Restore with
# scripts/restore.sh. docs/adr/0022-metrics-backups.md
set -eu
cd "$(dirname "$0")/.."
COMPOSE="docker compose --env-file .env -f deploy/docker-compose.yml"
root="${1:-backups}"
keep="${RUSCORD_BACKUP_KEEP:-7}"
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$root/files" "$root/$stamp"
root_abs="$(cd "$root" && pwd)"

# Custom format: compressed, restorable table by table, checked by pg_restore --list.
$COMPOSE exec -T postgres pg_dump -U ruscord -d ruscord -Fc > "$root/$stamp/postgres.dump.part"
mv "$root/$stamp/postgres.dump.part" "$root/$stamp/postgres.dump"

# The core itself copies the files (ruscord files-backup), as the host user so
# the backup stays readable.
$COMPOSE --profile app run --rm --no-deps --user "$(id -u):$(id -g)" \
  -v "$root_abs/files:/backup" app files-backup /backup

(cd "$root/$stamp" && sha256sum postgres.dump > SHA256SUMS 2>/dev/null || shasum -a 256 postgres.dump > SHA256SUMS)
echo "backup: $root/$stamp (dump) and $root/files (files)"

# Keep the newest $keep dumps. Directory names sort by time.
ls -1d "$root"/????????T??????Z 2>/dev/null | sort |
  awk -v keep="$keep" '{ d[NR] = $0 } END { for (i = 1; i <= NR - keep; i++) print d[i] }' | while read -r old; do
  rm -rf "$old"
  echo "removed old backup $old"
done
