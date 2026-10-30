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
Caddy/nginx ──► api ──► Postgres, S3 (файлы)    LiveKit SFU (+TURN)
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
→ POST /api/v1/gateway/ticket   (cookie-сессия) ← { ticket, expires_in: 30 }, одноразовый
→ WSS  /gateway?v=1
← HELLO { heartbeat_interval }
→ IDENTIFY { ticket }           или  RESUME { ticket, session_id, seq }
← READY { v, session_id, user, guilds, dms, relationships }   s = 1
← DISPATCH { t, s, d }          s растёт на единицу в рамках сессии
→ HEARTBEAT { последний s }     ← HEARTBEAT_ACK
← закрытие с кодом 4xxx: что делать дальше, решает код (docs/api.md)
позже: → PRESENCE_UPDATE, VOICE_STATE_UPDATE; ← VOICE_SERVER_UPDATE, read_states и voice_states в READY
```

Тикет передаётся в первом сообщении, а не в URL, чтобы не попадать в логи прокси.
Каждая сессия читает стрим `EVENTS` своим упорядоченным потребителем JetStream с
фильтром `user.<id>`, `guild.<id>` своих серверов и `channel.<id>` своих личек;
при входе в сервер или выходе потребитель пересоздаётся сразу после этого
события. Фильтр по правам работает на снимке сервера в памяти сессии (все
каналы, роли, свой участник), который обновляется самими событиями; изменения
видимости превращаются в `CHANNEL_CREATE`/`CHANNEL_DELETE`. RESUME работает из
буфера кадров на той же ноде в течение 2 минут после обрыва. Подробности и
причины: ADR-0016. Presence получают только друзья и участники общих серверов
(после шага 5).

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

Готово на шаге 4a: `guilds`, `roles`, `members`, `member_roles`, `channels`,
`permission_overwrites`, `outbox`. Записи в сервер сериализуются блокировкой
строки `guilds` и проверяют права в той же транзакции (ADR-0012). Модуль
`internal/guild` загружает входные данные для `internal/perm`, пользователей
получает через интерфейс `UserDirectory` из `internal/auth`.

Шаг 4b: `invites`; relay `internal/events` публикует outbox в JetStream
(стрим `EVENTS`, ADR-0013).

Шаг 4c-1: модуль `internal/social` с `relationships`, `dm_channels`,
`dm_recipients` (ADR-0014). Лички больше не хранятся в `channels`.

Шаг 4c-2: модуль `internal/message` с таблицей `messages` (ADR-0015). Права на
канал он получает от guild и social через интерфейсы, а сообщения удаляются
вместе с каналом через `hooks.ChannelsDeleted`.

Шаг 5a: модуль `internal/gateway` (ADR-0016). Читает guild (`Snapshots`,
`Snapshot`), social (лички и связи) и auth (тикеты, `SessionActive`) через
интерфейсы; своих таблиц нет. Отзыв сессии входа пишет в outbox внутреннее
событие `SESSION_REVOKE`.

Права для интерфейса считает сервер (ADR-0017): объект сервера несёт
`channel_permissions`, а gateway при их изменении присылает
`GUILD_PERMISSIONS_UPDATE`. Клиент правила прав не реализует.

Шаг 5b-1 (ADR-0018): эфемерные события «печатает» и статусы идут не через
outbox, а через обычные subject'ы NATS `eph.*`; gateway держит одну подписку на
subject на ноду и фильтрует их тем же router. Модуль `internal/presence` хранит
живые сессии в Valkey с TTL и публикует изменение видимого статуса друзьям и
серверам до 250 участников. Выбранный статус — `users.status` в auth.

Шаг 5b-2 (ADR-0019): файлы в S3-совместимом хранилище (`internal/platform/blob`,
в compose — SeaweedFS), картинки проходят `internal/platform/imaging`
(проверка, квадрат 256×256, перекодирование), отдаются по `/cdn/<key>`.
`users.avatar`, `users.bio`, `guilds.icon` хранят ключи; таблица `blob_garbage`
— очередь удаления, которую разбирает сборщик с арендой, а ссылки проверяет
модуль-владелец (`RefCheck`). Изменение профиля auth рассылает в серверы
пользователя (`USER_PROFILE_UPDATE`), список серверов берёт у guild через
интерфейс `GuildIDs`. Просмотр чужого профиля — в social (там известны связи и
лички).

## Права

Пакет `internal/perm` (ADR-0011), чистые функции без БД:

- `perm.Base(guild, member)` — права на уровне сервера;
- `perm.InChannel(guild, member, overwrites)` — права в канале;
- `perm.InDM(isRecipient, blocked)` — права в личке;
- `perm.Outranks`, `perm.CanAssignRole` — иерархия ролей;
- `perm.Voice(p)` — грант для токена LiveKit.

```
base = @everyone | OR(roles)
ADMINISTRATOR или владелец → ALL
perm = (base &^ ow[@everyone].deny) | ow[@everyone].allow
perm = (perm &^ OR(ow[roles].deny)) | OR(ow[roles].allow)
perm = (perm &^ ow[member].deny)    | ow[member].allow
нет VIEW_CHANNEL → 0; нет SEND_MESSAGES → без вложений и @everyone;
нет CONNECT → без голосовых прав; нет STREAM → без STREAM_HQ
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
