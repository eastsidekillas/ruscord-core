COMPOSE := docker compose --env-file .env -f deploy/docker-compose.yml
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: run build test lint test-docker up up-app down logs env backup

env: ## create .env with random local secrets (does not overwrite)
	@if [ -f .env ]; then echo ".env already exists"; else ./scripts/gen-env.sh > .env && echo "wrote .env"; fi

up: ## start local infrastructure
	$(COMPOSE) up -d

up-app: ## infrastructure + ruscord in a container
	$(COMPOSE) --profile app up -d --build

down:
	$(COMPOSE) --profile app down

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
