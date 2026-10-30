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

### Всё в Docker, чтобы попробовать

Нужен только Docker. Клиент
([ruscord-fe](https://github.com/eastsidekillas/ruscord-fe)) должен лежать рядом:

```
ruscord/
  ruscord-core/
  ruscord-fe/
```

```bash
cd ruscord-core
make env       # один раз
make up-all    # инфраструктура + ядро + клиент; первая сборка — несколько минут
```

Клиент — http://localhost:8081: nginx клиента отдаёт страницу и проксирует
`/api`, `/gateway`, `/cdn` в ядро на том же адресе. Другой путь к клиенту —
`RUSCORD_FE_DIR=/path/to/ruscord-fe make up-all`. После `git pull` повторите
`make up-all` — образы пересоберутся. `make down` останавливает всё, данные
(тома Docker) остаются.

Для разработки клиента удобнее `make up-app` и `npm start` в ruscord-fe:
клиент на http://localhost:4200 с перезагрузкой при правках.

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
