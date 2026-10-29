# ruscord-core

Ядро Ruscord: REST API, gateway (WebSocket) и голосовой сервис в одном Go-бинаре.
Медиа (голос, видео, демонстрация экрана) обслуживает self-hosted LiveKit.

## Быстрый старт

Нужны Go 1.26+, Docker с Compose v2, `openssl`.

```bash
make env      # создаёт .env со случайными локальными секретами
make up       # Postgres, Valkey, NATS, LiveKit на 127.0.0.1
make run      # запускает ruscord на хосте
curl -s localhost:8080/readyz
```

`make up-app` собирает и запускает ruscord в контейнере вместе с инфраструктурой.
`make test` и `make lint` делают то же, что CI.

## Структура

```
cmd/ruscord/           точка входа; RUSCORD_ROLES=all|api,gateway,voice
internal/config/       конфигурация из env, валидация секретов
internal/health/       /healthz и /readyz
internal/platform/     логгер, HTTP-сервер, подключения к Postgres/Valkey/NATS
deploy/                docker-compose, Dockerfile, конфиг LiveKit для разработки
docs/                  архитектура, ADR, план, журнал разработки
```

## Документация

Начинать с [docs/README.md](docs/README.md). Каждый PR обновляет журнал
разработки и, если меняет решение, соответствующий ADR.
