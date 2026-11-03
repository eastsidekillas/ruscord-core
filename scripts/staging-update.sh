#!/usr/bin/env bash
# Updates the test server from git (docs/deploy-staging.md): pulls
# ruscord-core and ruscord-fe, backs up the database and files (migrations
# run when the core starts and are not undone), rebuilds and restarts the
# stack, waits for the core and runs scripts/staging-check.sh.
#
#   scripts/staging-update.sh                 update if there is anything new
#   scripts/staging-update.sh --force         rebuild even without new commits
#   scripts/staging-update.sh --no-backup     skip the backup
#   scripts/staging-update.sh --dry-run       only show what would come
#
# ruscord-fe is expected next to this repository; RUSCORD_FE_DIR overrides.
# Both must be on their main branch without local changes: the script only
# fast-forwards and never overwrites anything.
set -euo pipefail
cd "$(dirname "$0")/.."
core=$(pwd)
fe=$(cd "${RUSCORD_FE_DIR:-../ruscord-fe}" && pwd)

force=0 backup=1 dry=0
for a in "$@"; do
  case "$a" in
    --force) force=1 ;;
    --no-backup) backup=0 ;;
    --dry-run) dry=1 ;;
    -h|--help) sed -n '2,15p' "$0"; exit 0 ;;
    *) echo "unknown option: $a (see --help)"; exit 2 ;;
  esac
done

say() { printf '\n== %s\n' "$*"; }
die() { printf 'stop: %s\n' "$*" >&2; exit 1; }

[ -f .env ] || die ".env is missing: make staging-env first"

# Fetches a repository and tells what is new; refuses local changes and
# other branches, so nothing of the server's own is lost.
declare -A before branch
prepare() { # name dir
  local name=$1 dir=$2
  local b
  b=$(git -C "$dir" rev-parse --abbrev-ref HEAD)
  case "$b" in main|master) ;; *) die "$name is on branch $b, want main or master" ;; esac
  [ -z "$(git -C "$dir" status --porcelain --untracked-files=no)" ] || die "$name has local changes: $(git -C "$dir" status --short --untracked-files=no | head -5 | tr '\n' ' ')"
  git -C "$dir" fetch -q origin "$b"
  git -C "$dir" merge-base --is-ancestor HEAD "origin/$b" || die "$name has commits that are not on GitHub; sort them out by hand"
  before[$name]=$(git -C "$dir" rev-parse HEAD)
  branch[$name]=$b
  local n
  n=$(git -C "$dir" rev-list --count "HEAD..origin/$b")
  if [ "$n" = 0 ]; then
    echo "$name: up to date (${before[$name]:0:7})"
  else
    echo "$name: $n new commit(s):"
    git -C "$dir" log --format='  %h %s' "HEAD..origin/$b"
  fi
  return 0
}

say "what is new"
prepare ruscord-core "$core"
prepare ruscord-fe "$fe"
new=$(( $(git -C "$core" rev-list --count "HEAD..origin/${branch[ruscord-core]}") + $(git -C "$fe" rev-list --count "HEAD..origin/${branch[ruscord-fe]}") ))

if [ "$dry" = 1 ]; then echo; echo "dry run: nothing changed"; exit 0; fi
if [ "$new" = 0 ] && [ "$force" = 0 ]; then echo; echo "nothing to update (--force rebuilds anyway)"; exit 0; fi

if [ "$backup" = 1 ]; then
  say "backup before the update"
  make --no-print-directory staging-backup
fi

say "pulling"
git -C "$core" merge -q --ff-only "origin/${branch[ruscord-core]}"
git -C "$fe" merge -q --ff-only "origin/${branch[ruscord-fe]}"

# Settings the template has and .env does not: their defaults apply, but
# they may need a value (as RUSCORD_LIVEKIT_MODE did).
missing=$(comm -23 <(sed -n 's/^\(RUSCORD_[A-Z0-9_]*\)=.*/\1/p' deploy/staging/env.example | sort -u) \
                   <(sed -n 's/^\(RUSCORD_[A-Z0-9_]*\)=.*/\1/p' .env | sort -u))
if [ -n "$missing" ]; then
  say "new settings in deploy/staging/env.example, not in .env"
  echo "$missing" | sed 's/^/  /'
  echo "  defaults apply; see the template's comments and add them to .env if needed"
fi

say "building and restarting"
make --no-print-directory staging-up

say "waiting for the core"
for _ in $(seq 1 90); do
  curl -fsS -o /dev/null --max-time 2 http://127.0.0.1:8080/readyz && break
  sleep 2
done

say "checking"
if ./scripts/staging-check.sh; then
  echo
  echo "updated: ruscord-core $(git -C "$core" rev-parse --short HEAD), ruscord-fe $(git -C "$fe" rev-parse --short HEAD)"
else
  cat <<EOF

The check failed. Logs: make staging-logs
To go back to the code before the update:
  git -C $core checkout -q ${before[ruscord-core]:0:12} && git -C $fe checkout -q ${before[ruscord-fe]:0:12} && make staging-up
Migrations are not undone by this: if the new code changed the database,
restore the backup made above (make staging-restore DIR=backups/<time>).
Return to the branches afterwards: git checkout ${branch[ruscord-core]} / ${branch[ruscord-fe]}.
EOF
  exit 1
fi
