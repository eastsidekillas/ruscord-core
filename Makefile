LIVEKIT_MODE := $(or $(shell sed -n 's/^RUSCORD_LIVEKIT_MODE=//p' .env 2>/dev/null | tail -1),local)
LIVEKIT_LOCAL_COMPOSE := $(if $(filter cloud,$(LIVEKIT_MODE)),,-f deploy/local-livekit.yml --profile local)
LIVEKIT_STAGING_COMPOSE := $(if $(filter cloud,$(LIVEKIT_MODE)),,-f deploy/staging/local-livekit.yml)
COMPOSE := docker compose --env-file .env -f deploy/docker-compose.yml $(LIVEKIT_LOCAL_COMPOSE)
# The test server (docs/deploy-staging.md): the same stack with deploy/staging on top.
STAGING := $(COMPOSE) -f deploy/staging/compose.yml $(LIVEKIT_STAGING_COMPOSE) --profile app --profile web --profile staging
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: run build test lint test-docker up up-app up-all down logs env backup staging-env staging-up staging-down staging-logs staging-backup staging-restore staging-check

env: ## create .env with random local secrets (does not overwrite)
	@if [ -f .env ]; then echo ".env already exists"; else ./scripts/gen-env.sh > .env && echo "wrote .env"; fi

up: ## start local infrastructure
	$(COMPOSE) up -d

up-app: ## infrastructure + ruscord in a container
	$(COMPOSE) --profile app up -d --build

up-all: ## infrastructure + ruscord + the web client (ruscord-fe next to this repo) on http://localhost:8081
	$(COMPOSE) --profile app --profile web up -d --build

down:
	$(COMPOSE) --profile app --profile web down

logs:
	$(COMPOSE) logs -f

run: ## run ruscord on the host against local infrastructure
	set -a && . ./.env && set +a && go run ./cmd/ruscord

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/ruscord ./cmd/ruscord

test:
	go test -race -count=1 ./...

lint:
	golangci-lint run

backup: ## Postgres dump and stored files into backups/ (stack up via make up-app)
	./scripts/backup.sh

test-docker: ## tests (-race, Postgres 18) and lint in Docker, like CI; log in test-docker.log
	./scripts/test-docker.sh

staging-env: ## the test server's .env with random secrets (does not overwrite); then set RUSCORD_DOMAIN
	@if [ -f .env ]; then echo ".env already exists"; else ./scripts/gen-env.sh deploy/staging/env.example > .env && echo "wrote .env: set RUSCORD_DOMAIN"; fi

staging-up: ## build and start the test server's stack (ruscord-fe next to this repo)
	$(STAGING) up -d --build --remove-orphans

staging-down:
	$(STAGING) down

staging-logs:
	$(STAGING) logs -f --tail=200

staging-backup: ## database dump and files into backups/ on the test server
	RUSCORD_COMPOSE="$(STAGING)" ./scripts/backup.sh

staging-restore: ## restore a backup on the test server: make staging-restore DIR=backups/<time> (stop the app first)
	RUSCORD_COMPOSE="$(STAGING)" ./scripts/restore.sh $(DIR)

staging-check: ## the site, the API, the gateway and LiveKit answer through HTTPS
	./scripts/staging-check.sh
