# Поддержка LiveKit Cloud: план реализации

> Ветка: `feat/livekit-cloud-mode` в `ruscord-core` и `ruscord-fe`.

**Цель:** переключать self-hosted LiveKit и LiveKit Cloud через `.env`, сохраняя локальный режим по умолчанию и не передавая API secret клиенту.

**Архитектура:** Core валидирует `RUSCORD_LIVEKIT_MODE`, использует `RUSCORD_LIVEKIT_URL` для серверных API и в Cloud по умолчанию возвращает этот адрес браузеру. Compose помещает self-hosted LiveKit в профиль `local`; Make подключает `deploy/local-livekit.yml` в режиме local согласно `.env` (в том числе переопределяя legacy loopback URL внутри контейнера). nginx получает только публичный URL на старте контейнера и генерирует CSP для этого URL. Staging Caddy отдельно проксирует webhook в Core.

**Стек:** Go, Docker Compose, Make, nginx envsubst, Angular.

## Ограничения

- `RUSCORD_LIVEKIT_MODE=local` остаётся значением по умолчанию.
- API Key и API Secret доступны только процессу Core и self-hosted LiveKit в local profile.
- Cloud webhook должен иметь валидную подпись; endpoint Core: `/livekit/webhook`.
- CSP добавляет только заданный `RUSCORD_LIVEKIT_PUBLIC_URL`; произвольные домены не допускаются.
- Ни один staging/production стек не запускается.

## Риски для ревью

- Compose `.env` interpolation и `env_file` имеют разные приоритеты: явный `environment` должен передавать URL Cloud в контейнер Core.
- Cloud Compose не включает local overlay: local service и его credentials отсутствуют в Cloud конфигурации, а не просто выключены профилем.
- В staging webhook публикуется через Caddy, а не через клиентский nginx; подпись LiveKit проверяется существующим handler.
- LiveKit Cloud URL должен быть `wss://` для клиентов и преобразуется в `https://` для Twirp API.
- nginx envsubst не должен затронуть nginx-переменные `$http_host` и `$uri`.

## Затрагиваемые файлы

- Core: `internal/config/config.go`, tests; `deploy/docker-compose.yml`, `deploy/local-livekit.yml`, `deploy/staging/compose.yml`, `deploy/staging/local-livekit.yml`, `deploy/staging/env.example`, `.env.example`, `Makefile`, `.github/workflows/ci.yml`; `deploy/staging/Caddyfile`; docs (`ADR`, staging guide, devlog, assumptions, API).
- Frontend: `nginx/default.conf` → `nginx/default.conf.template`, `Dockerfile`, `e2e/ci.sh`; CSP проверки.

## Этапы

1. Добавить табличные Go-тесты local/cloud mode, адреса клиента и валидации Cloud URL/credentials; затем реализовать нормализацию и валидацию config.
2. Изолировать локальный LiveKit в Compose profile `local`, сделать `make` выбирать profile по `.env`, передавать Cloud URL Core и публичный URL только nginx.
3. Добавить тест статической CSP-политики и валидацию URL в конфигурации; настроить runtime envsubst nginx с allowlist только `RUSCORD_LIVEKIT_PUBLIC_URL`.
4. Направить публичный staging webhook `https://<RUSCORD_DOMAIN>/livekit/webhook` из Caddy в `core:8080`; зафиксировать ограничение настройки webhook в Cloud Dashboard.
5. Обновить ADR, staging/local инструкции, `docs/api.md`, assumptions и devlog.
6. Проверить local/cloud `docker compose config`, staging в обоих режимах, Core `go test ./...`, frontend build и CSP host; затем собрать два PR.

## Проверки

- Core: `go test -race -count=1 ./...` (или более узкий тест config после красного цикла).
- Frontend: тесты CSP/static template при наличии и `npm run build --configuration=production`.
- Compose: конфигурации local, cloud, staging local, staging cloud; проверить профиль сервиса LiveKit, Core URL и отсутствие API secret в web.
- Webhook: проверить путь и проксирование маршрута в Caddy; unit tests подписи webhook уже существуют.
