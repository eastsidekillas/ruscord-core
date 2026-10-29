# Архитектура ядра

## Плоскости

- **Управление** (`api`): REST, источник истины в Postgres. Каждая запись пишет
  событие в таблицу `outbox` в той же транзакции.
- **События** (`gateway`, NATS JetStream): outbox-relay публикует события в NATS,
  gateway рассылает их по WebSocket тем сессиям, которым они положены.
- **Медиа** (LiveKit): WebRTC SFU. Ядро общается с ним только из модуля `voice`
  (токены, RoomService API, вебхуки).

```
Клиенты (Web/Angular, Desktop/Tauri)
  │ HTTPS / WSS                                 │ WebRTC (DTLS-SRTP, UDP или TURN 443)
  ▼                                             ▼
Caddy/nginx ──► api ──► Postgres                LiveKit SFU (+TURN)
           └──► gateway ◄── NATS ◄── outbox-relay   ▲   │ вебхуки
                  │  └──► Valkey (сессии, presence)  │   ▼
                  └──► voice ──── RoomService API ───┘  voice
```

## Модули

| Модуль | Отвечает за | Хранит |
|---|---|---|
| `api` | аккаунты, сессии, серверы, каналы, роли, сообщения, инвайты, связи | Postgres |
| `gateway` | одно WS-соединение на устройство: тикет, heartbeat, resume, подписки, рассылка, typing, presence | Valkey, буфер resume в памяти |
| `voice` | голосовые состояния, звонки в личках, проверка прав и выпуск токенов LiveKit, вебхуки, серверный mute/kick/move | Valkey с TTL, история звонков в Postgres |
| `workers` | outbox-relay, превью, пуши, письма | — |

Модули живут в одном бинаре (`RUSCORD_ROLES`) и могут быть вынесены в отдельные
деплои без переписывания. Модуль не читает таблицы другого модуля напрямую.

## Протокол gateway (v1)

```
→ POST /gateway/ticket          (cookie-сессия) ← { ticket, ttl: 30s, одноразовый }
→ WSS  /gateway?v=1
→ IDENTIFY { ticket, device }   или  RESUME { session_id, seq }
← HELLO { heartbeat_interval }
← READY { session_id, user, guilds, dms, relationships, read_states, voice_states }
← DISPATCH { t, s, d }          s монотонно растёт в рамках сессии
→ HEARTBEAT { seq }             ← HEARTBEAT_ACK
→ PRESENCE_UPDATE, VOICE_STATE_UPDATE
← VOICE_SERVER_UPDATE { endpoint, token, room }
← RECONNECT, INVALID_SESSION
```

Тикет передаётся в первом сообщении, а не в URL, чтобы не попадать в логи прокси.
Нода gateway подписывается в NATS на `guild.<id>` для серверов своих сессий и на
`user.<id>` для личных событий. Presence получают только друзья и участники общих
серверов.

## Модель данных (целевая)

```
users, sessions(refresh_hash, family_id, revoked_at)
guilds, roles(permissions bigint), members, member_roles
channels(type: text|voice|category|dm|group_dm, parent_id, user_limit)
permission_overwrites(channel_id, target_type, target_id, allow, deny)
dm_recipients, messages(id uuidv7, channel_id, author_id, content, nonce, ...)
attachments, read_states, relationships, invites, calls, outbox
эфемерно в Valkey: presence:*, voice_state:*, call:*, ratelimit:*
```

Индекс `messages (channel_id, id DESC)`, курсорная пагинация before/after.

## Права

Одна чистая функция `ComputePermissions(member, channel)` в `internal/perm`:

```
base = @everyone | OR(roles)
ADMINISTRATOR или владелец → ALL
perm = (base &^ ow[@everyone].deny) | ow[@everyone].allow
perm = (perm &^ OR(ow[roles].deny)) | OR(ow[roles].allow)
perm = (perm &^ ow[member].deny)    | ow[member].allow
нет VIEW_CHANNEL → 0
```

Соответствие правам LiveKit: `CONNECT` → roomJoin, `SPEAK` → microphone,
`VIDEO` → camera, `STREAM` → screen_share + screen_share_audio. `roomAdmin`
клиенты не получают никогда; mute/move делает сервер. Имя и аватар участника
ставит сервер, `canUpdateOwnMetadata = false`.

## Ключевые потоки

**Звонок в личке.** `POST /channels/{dm}/call` создаёт `call:{id}` в Valkey
(ringing, TTL 45 с) и рассылает `CALL_CREATE` всем устройствам собеседника.
Комната LiveKit называется `call:<call_id>`, поэтому старые токены к новым
звонкам не подходят. Конец звонка определяется вебхуком `room_finished`, в чат
пишется системное сообщение.

**Голосовой канал.** `VOICE_STATE_UPDATE` через gateway → `voice` проверяет
`CONNECT` и `user_limit`, убирает пользователя из прошлого канала → отдаёт
`VOICE_SERVER_UPDATE` с токеном (TTL на вход 60 с) → рассылает голосовое
состояние участникам сервера.

**Сообщение.** `POST /channels/{id}/messages {content, nonce}` → проверка прав →
одна транзакция: messages + outbox → NATS → `MESSAGE_CREATE` с `nonce` для
дедупликации оптимистичного сообщения на клиенте.

## LiveKit в проде (на будущее, для staging)

Отличия от `deploy/livekit.dev.yaml`: host network, публичный IP
(`use_external_ip: true`), встроенный TURN на 443/TLS и 3478/UDP с отдельным
доменом, Redis/Valkey для мультинода, вебхуки на `voice` с проверкой подписи,
ключи из секретов, закреплённая версия образа, поднятые UDP-буферы ядра
(`net.core.rmem_max`, `wmem_max`). Требования к железу на старт: 4–8 vCPU,
8 ГБ RAM, канал от 1 Гбит/с.
