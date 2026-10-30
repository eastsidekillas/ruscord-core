#!/usr/bin/env bash
# Runs what CI runs, in Docker, so a machine without Go or Postgres can check
# a branch: Postgres 18 from the compose stack, the whole suite with -race in
# a Go container, then golangci-lint at the CI version. Everything is also
# written to test-docker.log.
#
#   make test-docker
set -euo pipefail
cd "$(dirname "$0")/.."

GO_IMAGE=golang:1.26
LINT_IMAGE=golangci/golangci-lint:v2.15.0 # keep in step with .github/workflows/ci.yml
LOG=test-docker.log
: >"$LOG"

[ -f .env ] || ./scripts/gen-env.sh >.env
compose() { docker compose --env-file .env -f deploy/docker-compose.yml "$@"; }
pg_password=$(sed -n 's/^RUSCORD_PG_PASSWORD=//p' .env)

# Integration tests only need Postgres: Valkey and NATS run in-process.
compose up -d --wait postgres 2>&1 | tee -a "$LOG"
if ! compose exec -T postgres psql -U ruscord -d ruscord -tAc \
	"SELECT 1 FROM pg_database WHERE datname = 'ruscord_test'" | grep -q 1; then
	compose exec -T postgres createdb -U ruscord ruscord_test
fi

# Module and build caches survive between runs in named volumes.
run_in() {
	local image=$1
	shift
	docker run --rm --network ruscord_default \
		-v "$PWD":/src -w /src \
		-v ruscord-gomod:/go/pkg/mod -v ruscord-gocache:/root/.cache \
		-e RUSCORD_TEST_POSTGRES_URL="postgres://ruscord:${pg_password}@postgres:5432/ruscord_test?sslmode=disable" \
		"$image" "$@"
}

status=0
echo "== go vet + go test -race ($GO_IMAGE)" | tee -a "$LOG"
run_in "$GO_IMAGE" sh -c 'go vet ./... && go test -race -count=1 ./...' 2>&1 | tee -a "$LOG" || status=1
echo "== golangci-lint ($LINT_IMAGE)" | tee -a "$LOG"
run_in "$LINT_IMAGE" golangci-lint run ./... 2>&1 | tee -a "$LOG" || status=1

if [ "$status" -eq 0 ]; then
	echo "== OK" | tee -a "$LOG"
else
	echo "== FAILED, see $LOG" | tee -a "$LOG"
fi
exit "$status"
