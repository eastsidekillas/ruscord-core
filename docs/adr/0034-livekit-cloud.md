# ADR-0034: LiveKit Cloud как альтернативный медиасервер

- Статус: принято
- Дата: 2026-11-02

## Контекст

Self-hosted LiveKit остаётся рабочим вариантом по умолчанию. Для запуска на
Cloud нельзя менять Compose вручную или собирать отдельный frontend: Core
должен использовать Cloud URL и credentials, клиент — публичный Cloud URL,
а секреты — оставаться на серверной стороне. Cloud отправляет webhook только
на публичный HTTPS endpoint.

## Решение

`RUSCORD_LIVEKIT_MODE=local|cloud` выбирает способ запуска. В local Core в
контейнере ходит на `ws://livekit:7880`, local LiveKit включён Compose profile
`local`, а браузер использует same-origin `/livekit`. В cloud Core использует
`RUSCORD_LIVEKIT_URL` (`wss://...`), выпускает токены теми же API key/secret и
возвращает Cloud URL клиенту; публичный URL по умолчанию равен server URL.
Frontend nginx получает только публичный URL и добавляет только его в
`connect-src`. В staging Caddy отправляет `/livekit/webhook` напрямую в Core.

## Альтернативы

- Отдельные Compose файлы для Cloud — дублировали бы стек.
- Проксировать Cloud сигналинг через frontend nginx — лишний сетевой hop и
  смешение Cloud URL с local fallback.
- Передавать API credentials клиенту — запрещено: браузеру нужен только
  временный join token.

## Последствия

- Переключение делается значением `.env`; self-hosted LiveKit не стартует в
  cloud mode.
- Cloud webhook настраивается отдельно в LiveKit Cloud Dashboard. Для staging
  endpoint — `https://<RUSCORD_DOMAIN>/livekit/webhook`; маршрут ведёт прямо
  на `/livekit/webhook` Core и проверяет JWT-подпись существующим handler.
- Переключение меняет backend-провайдера сразу для новых соединений; активные
  комнаты и участники не мигрируют между провайдерами.
- Cloud стоимость, размещение данных, TURN и SLA зависят от аккаунта LiveKit;
  это не проверяется Compose.

## Пересмотреть, если

- понадобится одновременно обслуживать комнаты через local и Cloud;
- появятся требования к Cloud webhook IP allowlist или отдельному webhook secret;
- нужно будет разрешить URL с path или несколькими Cloud origins.
