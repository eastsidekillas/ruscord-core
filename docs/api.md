# HTTP API

Базовый путь `/api/v1`. Тела запросов и ответов в JSON. Неизвестные поля в
запросе отклоняются (400). Ошибки всегда имеют вид:

```json
{ "code": "validation_failed", "message": "Проверьте поля формы", "fields": { "email": "Некорректный email" } }
```

`code` стабилен и предназначен для клиента, `message` для человека.

## Аутентификация

Токены живут только в cookie, JavaScript их не видит.

| Cookie | Что | Path | Время жизни |
|---|---|---|---|
| `rc_at` | access JWT (EdDSA) | `/` | 10 минут |
| `rc_rt` | refresh-токен | `/api/v1/auth` | 30 дней, продлевается при каждом обновлении |

Обе: `HttpOnly`, `SameSite=Lax`, `Secure` везде, кроме dev. Запросы с
изменением данных с чужим `Origin` отклоняются (403 `bad_origin`); разрешённые
origin задаёт `RUSCORD_ALLOWED_ORIGINS`.

Клиент при ответе 401 `unauthorized` на любом запросе вызывает
`POST /auth/refresh` и повторяет запрос. Ответ 401 `session_expired` от refresh
означает, что нужно войти заново.

| Метод и путь | Тело | Ответ | Ошибки |
|---|---|---|---|
| `POST /auth/register` | `{email, username, display_name?, password}` | 201, пользователь, ставит cookie | 422 `validation_failed`, 409 `email_taken` / `username_taken`, 429 |
| `POST /auth/login` | `{email, password}` | 200, пользователь, ставит cookie | 401 `invalid_credentials`, 429 |
| `POST /auth/refresh` | — (cookie `rc_rt`) | 200, пользователь, новые cookie | 401 `session_expired`, 429 |
| `POST /auth/logout` | — (cookie `rc_rt`) | 204, стирает cookie | — |
| `GET /users/@me` | — | 200, пользователь | 401 |
| `POST /gateway/ticket` | — | 200 `{ticket, expires_in: 30}` | 401, 429 |

Пользователь: `{id, email, username, display_name, created_at}`.

Правила полей: `username` от 2 до 32 символов из латиницы, цифр, `_` и `.`, без
`..`, уникален без учёта регистра; `display_name` от 1 до 32 символов, по
умолчанию равен `username`; `password` от 8 до 128 символов и не равен email или
username; email уникален без учёта регистра.

Лимиты: вход 20/мин с IP и 5/мин на email, регистрация 5/час с IP, refresh
30/мин на сессию, тикеты 30/мин на сессию.

## Серверы, каналы, роли, участники

Все запросы требуют `rc_at`. Если сервера нет или вы не участник, ответ 404
`not_found`; так же для канала без `VIEW_CHANNEL`. 403 `forbidden` означает,
что объект виден, но прав на действие нет. Права (`permissions`, `allow`,
`deny`) передаются строкой с десятичным числом, биты описаны в
`internal/perm/perm.go` и ADR-0011.

| Метод и путь | Тело | Ответ | Права |
|---|---|---|---|
| `GET /users/@me/guilds` | — | 200, `[guild]` | — |
| `POST /guilds` | `{name}` | 201, полный сервер | лимит 100 серверов на пользователя |
| `GET /guilds/{id}` | — | 200, полный сервер | участник |
| `PATCH /guilds/{id}` | `{name?}` | 200, guild | `MANAGE_GUILD` |
| `DELETE /guilds/{id}` | — | 204 | только владелец |
| `GET /guilds/{id}/channels` | — | 200, видимые каналы | участник |
| `POST /guilds/{id}/channels` | `{type, name, topic?, parent_id?, user_limit?, bitrate?}` | 201, канал | `MANAGE_CHANNELS` |
| `PATCH /channels/{id}` | `{name?, topic?, parent_id?, position?, user_limit?, bitrate?}` | 200, канал | `MANAGE_CHANNELS` в канале |
| `DELETE /channels/{id}` | — | 204 | `MANAGE_CHANNELS` в канале |
| `PUT /channels/{id}/permissions/{target_id}` | `{type: 1 роль / 2 участник, allow?, deny?}` | 200, канал | `MANAGE_ROLES` в канале, только свои биты |
| `DELETE /channels/{id}/permissions/{target_id}` | — | 204 | то же |
| `GET /guilds/{id}/roles` | — | 200, роли по возрастанию позиции | участник |
| `POST /guilds/{id}/roles` | `{name, permissions?, color?}` | 201, роль на позиции 1 | `MANAGE_ROLES`, только свои биты |
| `PATCH /guilds/{id}/roles/{role_id}` | `{name?, permissions?, color?, position?}` | 200, роль | роль ниже вашей высшей до и после изменения |
| `DELETE /guilds/{id}/roles/{role_id}` | — | 204 | роль ниже вашей высшей |
| `GET /guilds/{id}/members?after=&limit=` | — | 200, участники по `user_id`, до 1000 | участник |
| `PATCH /guilds/{id}/members/@me` | `{nick}` (`null` или `""` сбрасывает) | 204 | `CHANGE_NICKNAME` |
| `DELETE /guilds/{id}/members/@me` | — | 204, выход с сервера | владелец не может выйти (400 `owner_cannot_leave`) |
| `DELETE /guilds/{id}/members/{user_id}` | — | 204, кик | `KICK_MEMBERS` и ваша высшая роль выше |
| `PUT /guilds/{id}/members/{user_id}/roles/{role_id}` | — | 204 | можете выдать роль и выше участника (или это вы) |
| `DELETE /guilds/{id}/members/{user_id}/roles/{role_id}` | — | 204 | то же |

Полный сервер: `{id, name, owner_id, created_at, roles, channels, member, permissions}`,
где `channels` только видимые вам, `member` ваше участие, `permissions` ваши
права на уровне сервера (без переопределений каналов).

Канал: `{id, guild_id, type, name, topic?, parent_id?, position, user_limit?, bitrate?, permission_overwrites}`.
Типы: 0 текст, 2 голос, 4 категория. `user_limit` (0–99) и `bitrate` (8000–384000,
по умолчанию 64000) только у голосовых. Категории не вкладываются.

Роль: `{id, guild_id, name, position, permissions, color}`. У @everyone id сервера
и позиция 0. Его нельзя удалить, переименовать, переместить или выдать
(400 `everyone_role`, у каждого случая свой текст), но можно менять его права.

Участник: `{guild_id, user_id, nick, roles, joined_at, user: {id, username, display_name}}`.

События (уходят через gateway с шага 5): `GUILD_CREATE`, `GUILD_UPDATE`,
`GUILD_DELETE`, `CHANNEL_CREATE/UPDATE/DELETE`, `GUILD_ROLE_CREATE/UPDATE/DELETE`
(с полным списком ролей, потому что позиции сдвигаются), `GUILD_MEMBER_ADD/UPDATE/REMOVE`.
Изменение переопределений прав приходит как `CHANNEL_UPDATE`.

Лимиты частоты на пользователя: создание сервера 10/час, канала 30/мин, роли
30/мин, приглашения 20/мин. Ответ 429 `rate_limited`.

## Приглашения

| Метод и путь | Тело | Ответ | Права |
|---|---|---|---|
| `POST /channels/{id}/invites` | `{max_age?, max_uses?}` (можно `{}`) | 201, приглашение | `CREATE_INVITE` в канале; не для категорий |
| `GET /guilds/{id}/invites` | — | 200, все приглашения сервера | `MANAGE_GUILD` |
| `GET /invites/{code}` | — | 200, превью | любой вошедший пользователь |
| `POST /invites/{code}` | — | 200, полный сервер | любой вошедший пользователь |
| `DELETE /invites/{code}` | — | 204 | создатель, `MANAGE_CHANNELS` в канале или `MANAGE_GUILD` |

- `max_age` в секундах: по умолчанию 86400 (сутки), 0 бессрочно, максимум 604800 (неделя).
- `max_uses` от 0 (без ограничений) до 100.
- Просроченный или исчерпанный код отвечает 404, как и несуществующий. Так же отвечает код, чей канал новичок (только @everyone) сейчас не видит.
- Повторный `POST /invites/{code}` от участника сервера отвечает 200 и не тратит использование.
- Просмотр и вход по коду вместе ограничены 30/мин на пользователя, чтобы коды нельзя было перебирать.

Приглашение: `{code, guild_id, channel_id, inviter_id, inviter?, max_uses, uses, expires_at, created_at}`.
Превью: `{code, guild: {id, name}, channel: {id, name, type}, inviter?, member_count, expires_at}`.
Код: 8 символов `[A-Za-z0-9]`.

События: `INVITE_CREATE`, `INVITE_DELETE` в топик сервера; gateway отдаёт их
только тем, у кого `MANAGE_GUILD` (шаг 5). Вход по приглашению порождает
`GUILD_MEMBER_ADD` и личный `GUILD_CREATE`.
