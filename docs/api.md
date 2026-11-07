# HTTP API

Базовый путь `/api/v1`. Тела запросов и ответов в JSON. Неизвестные поля в
запросе отклоняются (400). Ошибки всегда имеют вид:

```json
{ "code": "validation_failed", "message": "Проверьте поля формы", "fields": { "email": "Некорректный email" } }
```

`code` стабилен и предназначен для клиента, `message` для человека.

Тело запроса не больше 1 МиБ, иначе 413 `body_too_large`. Битый JSON, пустое
тело, неверный тип поля или неизвестное поле: 400 `invalid_request`, в
`message` названо поле. Неверный `limit` или id в query-параметрах: 400
`invalid_request`.

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
| `POST /auth/register` | `{email, username, display_name?, password, ref?}` (`ref` — метка ссылки `?ref=`, неподходящая отбрасывается) | 201, пользователь, ставит cookie | 422 `validation_failed`, 409 `email_taken` / `username_taken`, 429 |
| `POST /auth/login` | `{email, password}` | 200, пользователь, ставит cookie | 401 `invalid_credentials`, 429 |
| `POST /auth/refresh` | — (cookie `rc_rt`) | 200, пользователь, новые cookie | 401 `session_expired`, 429 |
| `POST /auth/logout` | — (cookie `rc_rt`) | 204, стирает cookie | — |
| `GET /users/@me` | — | 200, пользователь | 401 |
| `POST /gateway/ticket` | — | 200 `{ticket, expires_in: 30}` | 401, 429 |
| `PATCH /users/@me` | `{display_name?, bio?, status?}` | 200, пользователь | 401, 422, 429 |
| `PUT /users/@me/avatar` | картинка (тело запроса) | 200, пользователь | 401, 413, 422, 429, 503 |
| `DELETE /users/@me/avatar` | — | 200, пользователь | 401, 429 |
| `GET /users/{id}/profile` | — | 200, `{user, bio}` | 401, 404 |
| `DELETE /users/@me` | `{password}` | 204, стирает cookie | 401, 403 `wrong_password`, 400 `owns_guilds`, 429 |
| `GET /users/@me/sessions` | — | 200, `[session]` | 401 |
| `DELETE /users/@me/sessions/{id}` | — | 204; своя текущая — стирает cookie | 401, 404 |
| `POST /users/@me/sessions/revoke-others` | `{password}` | 200, `{revoked: n}` | 401, 403 `wrong_password`, 429 |
| `POST /users/@me/password` | `{current_password, new_password}` | 204 | 401, 403 `wrong_password`, 422, 429 |

Пользователь: `{id, email, username, display_name, created_at, status, avatar, bio}`.
Публичный пользователь (в участниках, связях, личках, профиле):
`{id, username, display_name, avatar, deleted?}`. У удалённого аккаунта
`deleted: true`, `display_name` «Удалённый пользователь», без имени и аватара.

Удаление аккаунта (ADR-0021): пароль ещё раз; владелец серверов сначала
удаляет их. Сразу закрываются все сессии и сокеты; аккаунт выходит из
серверов, групп и друзей; переписки и сообщения остаются от «удалённого
пользователя».

Пароль ещё раз спрашивают удаление аккаунта, «выйти на всех других
устройствах» и смена пароля; вместе — 5 попыток в час на аккаунт.

Сессия (устройство, ADR-0023): `{id, device, ip, created_at, last_used_at,
current}`. `device` — User-Agent при входе, `ip` — адрес последнего обновления
токена (или входа), `current` — сессия этого запроса. Список — только
активные, недавно использованные сверху. Завершённая сессия перестаёт работать
сразу, а не когда истечёт её access-токен; её сокеты закрываются. Смена
пароля завершает все другие сессии. Активных сессий не больше 100: вход на
101-м устройстве завершает давно не использованную.

`PATCH /users/@me` меняет только переданные поля (`null` = не передано), нужно
хотя бы одно: `display_name` 1–32 символа, `bio` («о себе») до 190 символов,
пробелы по краям обрезаются. 10 изменений в минуту.

`status` — статус, который пользователь выбрал: `online`, `idle`, `dnd` или
`invisible`. Другие видят `offline`, если вы не в сети или выбрали `invisible`
(см. «Статусы» в разделе gateway). Смена статуса: 10 в минуту; изменение
приходит вашим сессиям событием `USER_UPDATE` с полным объектом пользователя.

### Аватар и иконка сервера

Картинка отправляется телом запроса как есть (любой `Content-Type`), до 8 МиБ
(больше — 413 `body_too_large`). Принимаются PNG, JPEG, WebP и GIF (первый
кадр), не больше 4096 пикселей по стороне; иначе 422 `invalid_image` или
`image_too_large`. SVG и анимированный WebP не принимаются. Сервер вырезает
центральный квадрат и сохраняет 256×256 (PNG при прозрачности, иначе JPEG),
метаданные (EXIF) удаляются. Лимит — 5 загрузок за 10 минут на пользователя,
аватары и иконки вместе. 503 `files_unavailable` — хранилище не настроено.

`avatar` (и `icon` у сервера) — путь вида `/cdn/avatars/<id>/<hash>.png` или
`null`. Файл по пути никогда не меняется, его можно кешировать навсегда; новый
аватар — новый путь. Старый файл доступен ещё сутки.

Изменение имени или аватара приходит участникам ваших серверов событием
`USER_PROFILE_UPDATE` (см. gateway).

`GET /users/{id}/profile` (или `@me`) доступен, если у вас с человеком общий
сервер, общая личка или группа, или связь (друг, заявка, ваша блокировка).
Иначе 404, как для несуществующего.

Правила полей: `username` от 2 до 32 символов из латиницы, цифр, `_` и `.`, без
`..`, уникален без учёта регистра; `display_name` от 1 до 32 символов, по
умолчанию равен `username`; `password` от 8 до 128 символов и не равен email или
username; email уникален без учёта регистра.

Лимиты: вход 20/мин с IP и 5/мин на email, регистрация 5/час с IP (за прокси — адрес клиента, ADR-0020), refresh
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
| `PUT /guilds/{id}/icon` | картинка (тело запроса) | 200, guild | `MANAGE_GUILD`; правила — «Аватар и иконка сервера» |
| `DELETE /guilds/{id}/icon` | — | 200, guild | `MANAGE_GUILD` |
| `DELETE /guilds/{id}` | — | 204 | только владелец |
| `GET /guilds/{id}/channels` | — | 200, видимые каналы | участник |
| `POST /guilds/{id}/channels` | `{type, name, topic?, parent_id?, user_limit?, bitrate?}` | 201, канал | `MANAGE_CHANNELS` |
| `PATCH /channels/{id}` | `{name?, topic?, parent_id?, position?, user_limit?, bitrate?}` | 200, канал | `MANAGE_CHANNELS` в канале |
| `DELETE /channels/{id}` | — | 204 | `MANAGE_CHANNELS` в канале |
| `PUT /channels/{id}/permissions/{target_id}` | `{type: 1 роль / 2 участник, allow?, deny?}` | 200, канал | `MANAGE_ROLES` в канале, только свои биты; `ADMINISTRATOR` в канале нельзя (422) |
| `DELETE /channels/{id}/permissions/{target_id}` | — | 204 | то же |
| `GET /guilds/{id}/roles` | — | 200, роли по возрастанию позиции | участник |
| `POST /guilds/{id}/roles` | `{name, permissions?, color?, mentionable?}` | 201, роль на позиции 1 | `MANAGE_ROLES`, только свои биты |
| `PATCH /guilds/{id}/roles/{role_id}` | `{name?, permissions?, color?, position?, mentionable?}` | 200, роль | роль ниже вашей высшей до и после изменения |
| `DELETE /guilds/{id}/roles/{role_id}` | — | 204 | роль ниже вашей высшей |
| `GET /guilds/{id}/members?after=&limit=` | — | 200, участники по `user_id`, до 1000 | участник |
| `PATCH /guilds/{id}/members/@me` | `{nick}` (`null` или `""` сбрасывает) | 204 | `CHANGE_NICKNAME` |
| `DELETE /guilds/{id}/members/@me` | — | 204, выход с сервера | владелец не может выйти (400 `owner_cannot_leave`) |
| `DELETE /guilds/{id}/members/{user_id}` | — | 204, кик | `KICK_MEMBERS` и ваша высшая роль выше |
| `PUT /guilds/{id}/bans/{user_id}` | `{reason?}` (до 512) или без тела | 204, бан; участник выходит с сервера | `BAN_MEMBERS` и ваша высшая роль выше (если участник); себя — 400 |
| `DELETE /guilds/{id}/bans/{user_id}` | — | 204 | `BAN_MEMBERS` |
| `GET /guilds/{id}/bans` | — | 200, `[{user_id, user, reason, moderator_id, created_at}]`, новые первыми | `BAN_MEMBERS` |
| `PUT /guilds/{id}/members/{user_id}/roles/{role_id}` | — | 204 | можете выдать роль и выше участника (или это вы) |
| `DELETE /guilds/{id}/members/{user_id}/roles/{role_id}` | — | 204 | то же |

Сервер (`guild`): `{id, name, owner_id, created_at, icon}` (`icon` — путь `/cdn/...` или `null`).
Полный сервер: `{id, name, owner_id, created_at, icon, roles, channels, member, permissions, channel_permissions}`,
где `channels` только видимые вам, `member` ваше участие (с `user`), `permissions` ваши
права на уровне сервера, `channel_permissions` — `{channel_id: права}` для каждого
канала из `channels` с учётом переопределений. Права считает сервер
(`internal/perm`); клиент только проверяет биты, например «показывать поле
ввода» = `SEND_MESSAGES` в `channel_permissions[id]` (ADR-0017).

Канал: `{id, guild_id, type, name, topic?, parent_id?, position, user_limit?, bitrate?, permission_overwrites}`.
Типы: 0 текст, 2 голос, 4 категория. `user_limit` (0–99) и `bitrate` (8000–384000,
по умолчанию 64000) только у голосовых. Категории не вкладываются.

Роль: `{id, guild_id, name, position, permissions, color, mentionable}`;
`mentionable` — роль может упомянуть любой (ADR-0035), у @everyone не меняется (400). У @everyone id сервера
и позиция 0. Его нельзя удалить, переименовать, переместить или выдать
(400 `everyone_role`, у каждого случая свой текст), но можно менять его права.

Участник: `{guild_id, user_id, nick, roles, joined_at, user: {id, username, display_name}, status?}`.
`status` (`online`, `idle`, `dnd`, `offline`) есть только в `GET /guilds/{id}/members`
и только на серверах до 250 участников: на больших статусы участников в MVP не
показываются (ADR-0018).

События (уходят через gateway с шага 5): `GUILD_CREATE`, `GUILD_UPDATE`,
`GUILD_DELETE`, `CHANNEL_CREATE/UPDATE/DELETE`, `GUILD_ROLE_CREATE/UPDATE/DELETE`
(с полным списком ролей, потому что позиции сдвигаются), `GUILD_MEMBER_ADD/UPDATE/REMOVE`.
Изменение переопределений прав приходит как `CHANNEL_UPDATE`. Удаление роли
и уход или кик участника удаляют их переопределения, и каждый затронутый
канал тоже приходит как `CHANNEL_UPDATE`.

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
Превью: `{code, guild: {id, name, icon?}, channel: {id, name, type}, inviter?, member_count, expires_at}`.
Код: 8 символов `[A-Za-z0-9]`.

События: `INVITE_CREATE`, `INVITE_DELETE` в топик сервера; gateway отдаёт их
только тем, у кого `MANAGE_GUILD` (шаг 5). Вход по приглашению порождает
`GUILD_MEMBER_ADD` и личный `GUILD_CREATE`.

## Друзья и блокировки

| Метод и путь | Тело | Ответ |
|---|---|---|
| `GET /users/@me/relationships` | — | 200, `[{id, type, since, user}]` |
| `POST /users/@me/relationships` | `{username}` | 204, заявка отправлена |
| `PUT /users/@me/relationships/{user_id}` | `{}` или `{type: 1}` | 204: заявка или принятие встречной |
| `PUT /users/@me/relationships/{user_id}` | `{type: 2}` | 204, блокировка |
| `DELETE /users/@me/relationships/{user_id}` | — | 204: удалить из друзей, отменить или отклонить заявку, разблокировать |

`type`: 1 друг, 2 заблокирован, 3 входящая заявка, 4 исходящая. `id` — другой
пользователь. Блокировка снимает дружбу и заявки; заблокированный этого не видит,
его заявка отвечает 400 `request_failed`, как на несуществующее имя. Свою
блокировку нужно снять перед заявкой (400 `user_blocked`). Лимиты: 1000 друзей и
исходящих заявок, 30 заявок в час.

События в личный топик: `RELATIONSHIP_ADD {id, type, since, user}` — в том же виде, что элемент списка, с публичным пользователем (заявку от незнакомого можно показать сразу), и `RELATIONSHIP_REMOVE {id, type}`.

## Личные чаты

| Метод и путь | Тело | Ответ |
|---|---|---|
| `GET /users/@me/channels` | — | 200, лички и группы, новые первыми; у канала с идущим звонком — `call` (ADR-0032) |
| `POST /users/@me/channels` | `{recipient_id}` | 200, личка 1:1 (создаётся при первом вызове) |
| `POST /users/@me/channels` | `{recipients: [id], name?}` | 200, новая группа |
| `PATCH /users/@me/channels/{id}` | `{name}` (`""` или `null` сбрасывает) | 200, группа |
| `DELETE /users/@me/channels/{id}` | — | 204, выйти из группы |
| `PUT /channels/{id}/recipients/{user_id}` | — | 204, добавить друга в группу |
| `DELETE /channels/{id}/recipients/{user_id}` | — | 204, удалить из группы (владелец) или выйти |

Канал: `{id, type: 1 или 3, name?, owner_id?, created_at, recipients: [user]}`,
`recipients` включает и вас.

- Личку 1:1 можно открыть с другом или участником общего сервера, если никто
  никого не заблокировал; иначе 403 `cannot_dm` без уточнения причины. Личка с
  самим собой: 400 `self`, как и заявка в друзья себе.
- Группа: от 1 до 9 друзей плюс владелец, добавлять можно только своих друзей
  (400 `not_friends`), всего не больше 10 (400 `group_full`). Если владелец
  выходит, группа переходит к тому, кто в ней дольше всех; последний вышедший
  удаляет её. Создание групп: 10 в час.
- Чужая или несуществующая личка отвечает 404.

События: `CHANNEL_CREATE`, `CHANNEL_UPDATE`, `CHANNEL_DELETE` (вам, когда вы
вышли или вас удалили), `CHANNEL_RECIPIENT_ADD/REMOVE {channel_id, user, channel}`
в личные топики участников.

## Сообщения

Одни и те же пути для текстовых каналов серверов и для личек.

| Метод и путь | Тело | Ответ | Права |
|---|---|---|---|
| `POST /channels/{id}/attachments?filename=` | файл (тело), `Content-Length` | 201, вложение без `url` | `SEND_MESSAGES` и `ATTACH_FILES` |
| `POST /channels/{id}/messages` | `{content, nonce?, reply_to?, reply_mention?, attachments?}` | 201, сообщение; 200 при повторе с тем же `nonce` | `SEND_MESSAGES` |
| `GET /channels/{id}/messages?before=&after=&limit=` | — | 200, сообщения, новые первыми | `READ_MESSAGE_HISTORY` |
| `PATCH /channels/{id}/messages/{message_id}` | `{content}` | 200, сообщение | автор и `SEND_MESSAGES` сейчас |
| `DELETE /channels/{id}/messages/{message_id}` | — | 204 | автор или `MANAGE_MESSAGES` (в личках только автор) |

Сообщение: `{id, channel_id, guild_id?, author: {id, username, display_name, avatar}, content,
nonce?, reply_to?, mention_everyone, mentions, mention_roles, attachments, created_at, edited_at?}`.

Вложение: `{id, filename, size, content_type, width?, height?, url}` (ADR-0036).

- Файл загружается до сообщения: тело запроса — сам файл, имя — в
  `?filename=` (путь отбрасывается, управляющие символы удаляются, до 255
  символов), размер — в `Content-Length` (без него 411). Больше 25 МиБ — 413,
  пустой — 422.
- Тип определяет сервер по содержимому. `content_type` image/png, image/jpeg,
  image/gif, image/webp (с `width` и `height`), video/mp4, video/webm —
  показываются в ленте; всё остальное (в том числе SVG и HTML) — файл для
  скачивания, `content_type` здесь только для значка.
- В сообщение идут до 10 своих загрузок в этот канал, не старше 50 минут и
  ещё не отправленных; иначе 422 `attachments`. С вложениями `content` может
  быть пустым; правкой текст можно стереть, если файлы есть.
- `url` — подписанная ссылка `/cdn/attachments/…?name=&ex=&sig=`, живёт
  24–36 часов; история и события приходят со свежими. Просроченная или
  чужая ссылка — 404. Поддерживается `Range` (206), картинки и видео
  отдаются `inline`, остальное — `attachment` с именем файла.
- Лимиты: 100 загрузок и 1 ГиБ в час на пользователя.
- Файлы удаляются с сообщением, каналом, сервером или группой (через сутки),
  неотправленные — через час.

- `content`: от 1 до 4000 символов после обрезки пробелов. `nonce` — строка
  до 64 байт от клиента; повтор с тем же nonce в том же канале не создаёт
  дубль и возвращает первое сообщение.
- `reply_to` должен быть сообщением этого же канала (иначе 422).
- `mention_everyone` = true, только если в тексте есть `@everyone` или `@here`
  и у автора есть `MENTION_EVERYONE`.
- Упоминания (ADR-0035) пишутся в тексте как `<@user_id>`, `<@&role_id>` и
  `<#channel_id>`; клиент показывает вместо них имена. Внутри кода (`` `…` ``,
  ```` ``` ````) упоминания и `@everyone` не пингуют.
  - `mentions` — упомянутые пользователи, которые есть в канале: участники
    сервера или получатели лички. Ответ добавляет автора исходного сообщения,
    если `reply_mention` не false (по умолчанию true) и это не сам автор.
  - `mention_roles` — роли сервера, которые автор может пинговать: с
    `mentionable` или любые при `MENTION_EVERYONE`. В личках всегда пусто.
  - Больше 50 разных упомянутых пользователей и ролей — 422.
  - Правка проверяет упоминания заново по текущим правам; пинг ответа
    сохраняется, пока исходное сообщение есть.
- История: `limit` от 1 до 100 (по умолчанию 50); `before` — старше этого id,
  `after` — новее этого id (ближайшие к нему), оба вместе нельзя.
- Голосовой канал или категория: 400 `not_text_channel`. Скрытый канал, чужая
  личка, чужой сервер: 404. В личке с блокировкой писать нельзя (403), читать можно.
- Лимит: 10 сообщений за 10 секунд на пользователя.
- Сообщения удаляются вместе с каналом, сервером или группой.
- Порядок: id сообщения (UUIDv7) задаёт порядок в канале. События от разных
  отправителей могут прийти в другом порядке, поэтому клиент вставляет
  сообщение по id, а не в конец списка.

События: `MESSAGE_CREATE`, `MESSAGE_UPDATE`, `MESSAGE_DELETE {id, channel_id, guild_id?}`
в топик сервера или `channel.<id>` для личек.

### Реакции и закрепления (ADR-0037)

| Метод и путь | Ответ | Права |
|---|---|---|
| `PUT /channels/{id}/messages/{message_id}/reactions/{emoji}/@me` | 204 | новый эмодзи — `ADD_REACTIONS`, уже стоящий — `READ_MESSAGE_HISTORY` (в личке всегда `ADD_REACTIONS`) |
| `DELETE /channels/{id}/messages/{message_id}/reactions/{emoji}/@me` | 204 | свою |
| `GET /channels/{id}/messages/{message_id}/reactions/{emoji}?limit=` | 200, пользователи, первые поставившие первыми (до 100, по умолчанию 25) | `READ_MESSAGE_HISTORY` |
| `DELETE /channels/{id}/messages/{message_id}/reactions/{emoji}` | 204 | `MANAGE_MESSAGES` |
| `DELETE /channels/{id}/messages/{message_id}/reactions` | 204 | `MANAGE_MESSAGES` |
| `GET /channels/{id}/pins` | 200, сообщения, последние закреплённые первыми | `READ_MESSAGE_HISTORY` |
| `PUT /channels/{id}/pins/{message_id}` | 204 | `PIN_MESSAGES` |
| `DELETE /channels/{id}/pins/{message_id}` | 204 | `PIN_MESSAGES` |

- `{emoji}` — один эмодзи Unicode в пути (URL-кодирование), с тоном кожи
  или без; иначе 422. Не больше 20 разных эмодзи под сообщением (422).
- Лимит: 20 реакций за 5 секунд на пользователя.
- В истории и закреплённых у сообщения `reactions: [{emoji, count, me}]` и
  `pinned_at`. В событиях сообщения `reactions: null` — клиент оставляет
  свои.
- События в топик канала: `MESSAGE_REACTION_ADD` и `MESSAGE_REACTION_REMOVE`
  `{channel_id, guild_id?, message_id, emoji, user_id}`,
  `MESSAGE_REACTION_REMOVE_EMOJI` `{…, emoji}`, `MESSAGE_REACTION_REMOVE_ALL`
  `{channel_id, guild_id?, message_id}`. Закрепление — `MESSAGE_UPDATE`.
- До 250 закреплённых в канале (422).

### Поиск (ADR-0039)

| Метод и путь | Ответ | Права |
|---|---|---|
| `GET /guilds/{id}/messages/search?…` | 200, `{total_results, messages}` | только каналы с `READ_MESSAGE_HISTORY` |
| `GET /channels/{id}/messages/search?…` | то же, в одном канале или личке | `READ_MESSAGE_HISTORY` |

- `q` — текст до 200 символов: формы слов по-русски и слова как написаны;
  `"фраза"`, `-слово`, `или` работают. Фильтры: `author_id`, `mentions`
  (упомянутый пользователь), `channel_id`, `has=file|image|video|link`
  (можно несколько раз), `before`, `after` (RFC 3339), `offset` (0–5000).
  Нужен текст или хотя бы один фильтр (иначе 422).
- Новые первыми, по 25 сообщений (как в истории, с реакциями). `total_results`
  — не больше 5025.
- Канал вне доступных в `channel_id` — пустой ответ, как несуществующий.
- Лимит: 1 поиск в секунду и 30 в минуту.
- Перейти к найденному: `GET /channels/{id}/messages?around=<message_id>&limit=` —
  половина страницы до сообщения (вместе с ним) и половина после;
  `around`, `before` и `after` вместе нельзя.

### Непрочитанное (ADR-0038)

| Метод и путь | Ответ | Права |
|---|---|---|
| `GET /users/@me/read-states` | 200, `[{channel_id, last_read_id, last_message_id, mention_count}]` | каналы с `READ_MESSAGE_HISTORY` и все лички |
| `POST /channels/{id}/messages/{message_id}/ack` | 204 | `READ_MESSAGE_HISTORY` |
| `POST /guilds/{id}/ack` | 204, все читаемые каналы сервера прочитаны | участник |

- `last_read_id` — последнее прочитанное сообщение (null — канал не
  открывали), `last_message_id` — последнее в канале (null — пусто).
  Непрочитанное есть, если `last_message_id` новее `last_read_id`.
- `mention_count` — сколько из последних 100 непрочитанных сообщений
  пингуют пользователя (по имени, ответом, его ролью, `@everyone`/`@here`);
  свои не считаются. Клиент показывает «99+».
- Отметка двигается только вперёд; своё отправленное сообщение сразу
  прочитано. Лимит: 30 отметок за 10 с, лишние молча игнорируются (204).
- Событие `MESSAGE_ACK {channel_id, message_id}` в топик пользователя, когда
  отметка сдвинулась: другие вкладки снимают непрочитанное.

### «Печатает»

`POST /channels/{id}/typing` → 204. Нужно право `SEND_MESSAGES` в канале (как
для отправки: 404 для скрытого и чужого канала, 400 `not_text_channel`, 403
без права или в личке с блокировкой). Клиент шлёт его при наборе не чаще раза в
5 с; более частые вызовы отвечают 204 и ничего не рассылают. Другие получают
`TYPING_START {channel_id, guild_id?, user_id, timestamp}` и показывают индикатор
10 с или до сообщения от того же автора.

Во всех текстовых полях символ NUL запрещён (422).

## Медленный режим и новые аккаунты (ADR-0043, ADR-0044)

- **Медленный режим.** У текстового канала есть `rate_limit_per_user`:
  секунды от 0 до 21600. Задаётся в `POST /guilds/{id}/channels` и
  `PATCH /channels/{id}`, нужно право `MANAGE_CHANNELS`.
  - Второе сообщение раньше срока — 429 `slow_mode` с `retry_after`.
  - Не действует на тех, у кого есть `MANAGE_MESSAGES` или
    `MANAGE_CHANNELS`.
- **Новые аккаунты.** Аккаунт моложе `RUSCORD_NEWCOMER_WAIT_MIN` минут
  получает 403 `account_too_new` с `retry_after`, если пишет в канал
  сервера, отправляет заявку в друзья или открывает личку с незнакомым.
- **Поле `retry_after`** (секунды) бывает в любой ошибке, которая проходит
  со временем; то же значение идёт в заголовке `Retry-After`.

## Статистика владельца (ADR-0042)

Только счётчики, без списков людей и содержимого.

- `POST /visits {ref?}` → 204, без входа. Считает заход на сайт по метке
  ссылки, один раз на адрес за 30 минут. Cookie и адреса не сохраняются.
- `GET /admin/stats?days=7..90` (по умолчанию 30) → 200. Доступен только
  id из `RUSCORD_ADMIN_USER_IDS`, остальным — 404. Ответ кешируется на 30 с.

  ```json
  {
    "generated_at": "…", "zone": "Europe/Moscow",
    "days": ["2026-10-07", "…", "2026-11-05"],
    "now": {"online": 0, "sessions": 0, "in_voice": 0, "in_calls": 0,
            "active_day": 0, "active_week": 0, "active_month": 0},
    "totals": {"users": 0, "guilds": 0, "messages": 0, "attachment_bytes": 0},
    "series": {"visits": [], "registrations": [], "active_users": [],
               "messages": [], "senders": [], "attachments": [],
               "guilds": [], "joins": []},
    "refs": [{"ref": "stream", "counts": {"visits": 0, "registrations": 0}}]
  }
  ```

  - `series` — одно число на каждый день из `days`.
  - `refs` — заходы и регистрации за период по меткам; `""` — без метки.
  - Активный день — день, когда пользователь открыл приложение (получил
    билет gateway).

## Голос (ADR-0025)

| Метод и путь | Тело | Ответ | Ошибки |
|---|---|---|---|
| `POST /channels/{id}/voice` | — | 200, `{url, token, room, sources, push_to_talk, high_quality}` | 400 `not_voice_channel`, 403 (нет `CONNECT`), 404, 409 `channel_full`, 429 |
| `PATCH /channels/{id}/voice` | `{self_mute, self_deaf}` | 200, голосовое состояние | 404, 409 `not_in_voice`, 429 |
| `PATCH /guilds/{id}/voice-states/{user}` | `{mute?, deaf?}` | 200, голосовое состояние | 403 (нет `MUTE_MEMBERS` / `DEAFEN_MEMBERS` в канале цели), 404 `not_in_voice`, 422, 429 |
| `DELETE /guilds/{id}/voice-states/{user}` | — | 204 | 403 (нет `MOVE_MEMBERS`), 404 `not_in_voice`, 429 |
| `POST /guilds/{id}/voice-states/{user}/move` | `{channel_id}` | 204 | 400 `not_voice_channel`, 403 (нет `MOVE_MEMBERS` в старом или новом канале, `CONNECT` в новом), 404 (`not_in_voice`, канал не найден), 409 `already_in_channel`, `cannot_move` (участник сам не может зайти), `channel_full`, 422, 429 |
| `POST /channels/{id}/call` | — | 200, `{url, token, room, sources, push_to_talk, high_quality, call}`: начинает звонок в личке или отвечает на идущий | 403 `cannot_call`, 404, 429 |
| `POST /channels/{id}/call/decline` | — | 204 | 404 (`no_call`, не ваша личка) |

Вход выдаёт токен LiveKit на 60 с. С ним клиент подключается к LiveKit:
`url` — его адрес, пустой `url` значит свой адрес под `/livekit` (nginx
клиента проксирует). `sources` — что можно публиковать: `microphone`,
`camera`, `screen_share`, `screen_share_audio`. Токен разрешает только их, и
LiveKit отклоняет остальное. `push_to_talk` — у пользователя нет
`USE_VOICE_ACTIVITY`. `high_quality` — есть `STREAM_HQ`: экран можно
показывать выше 720p; разрешение выбирает клиент, LiveKit его не ограничивает.
20 входов в минуту.

Голосовое состояние: `{user_id, guild_id, channel_id, self_mute, self_deaf, mute, deaf, user}`,
`user` — публичный профиль (нет у «вышел»). `mute` и `deaf` поставил
модератор (ADR-0026): у такого участника нет микрофона или входящего звука,
и выход с новым входом этого не снимает.
- Человек «в канале», когда LiveKit сообщил о подключении, а не сразу после
  выдачи токена.
- Голосовой канал один на пользователя: вход в другой выводит из прежнего.
- В объекте сервера (`GET /guilds/{id}`, `GUILD_CREATE`, READY) есть
  `voice_states` — только по видимым вам каналам.
- Кому нельзя быть в комнате (не участник, нет `CONNECT`), сервер выводит
  из неё — сразу после кика, бана или смены прав.
- Потеря `SPEAK`, `VIDEO` или `STREAM` действует сразу: LiveKit снимает
  лишние треки.
- Удаление голосового канала или сервера закрывает комнату.
- Свой mute и deafen — 30 в минуту, действия модератора — 60 в минуту.

## Gateway (WebSocket)

Одно соединение на вкладку или устройство: `wss://<хост>/gateway?v=1` (путь без
`/api/v1`). Браузерный `Origin` должен быть в `RUSCORD_ALLOWED_ORIGINS`.
Решения и их причины: ADR-0016.

Кадр — JSON-текст `{op, d, s?, t?}`; `s` и `t` есть только у DISPATCH.

| op | Направление | Что | `d` |
|---|---|---|---|
| 10 HELLO | ← | первый кадр после подключения | `{heartbeat_interval}` в мс |
| 2 IDENTIFY | → | новая сессия | `{ticket}` |
| 6 RESUME | → | продолжить сессию после обрыва | `{ticket, session_id, seq}` |
| 3 PRESENCE_UPDATE | → | устройство в работе или бездействует | `{status: "online" \| "idle"}` |
| 0 DISPATCH | ← | событие, `t` — тип, `s` — номер | объект события |
| 1 HEARTBEAT | → | раз в `heartbeat_interval` | последний полученный `s` или `null` (можно не передавать) |
| 11 HEARTBEAT_ACK | ← | ответ на каждый heartbeat | `null` |

Порядок:

1. `POST /api/v1/gateway/ticket` (тикет живёт 30 с и действует один раз),
   открыть сокет, дождаться HELLO.
2. В течение 10 с отправить IDENTIFY или RESUME с новым тикетом. Тикет
   передаётся в кадре, а не в URL, чтобы не попадать в логи прокси.
3. IDENTIFY → `READY` (`s = 1`): `{v, session_id, user, guilds, dms, relationships, presences}`.
   `guilds` — серверы в том же виде, что `GET /guilds/{id}` (без скрытых
   каналов), `dms` и `relationships` — как в REST, `presences` —
   `[{user_id, status}]` друзей, которые не `offline` (остальные — `offline`).
4. Дальше DISPATCH с `s`, растущим на единицу. Клиент хранит последний `s` и
   отправляет его в heartbeat: подтверждённые кадры сервер больше не хранит.
5. После обрыва: новый тикет, RESUME с `session_id` и последним `s`. Сервер
   повторяет пропущенные кадры с теми же номерами, затем DISPATCH `RESUMED`
   (без `s`). Сессия живёт 2 минуты после обрыва и только на той ноде, где была
   открыта; RESUME может сделать только та же сессия входа (то же устройство).

Heartbeat нужно слать каждые `heartbeat_interval` (30 с); без него 1,5
интервала соединение закрывается кодом 4009. Клиент может слать не больше 120
кадров в минуту и не больше 4 КиБ в кадре.

Коды закрытия:

| Код | Что | Что делать клиенту |
|---|---|---|
| 4000 | внутренняя ошибка, отказ записи | RESUME |
| 4001 | неизвестный op | RESUME (и исправить клиент) |
| 4002 | не JSON, бинарный кадр, неверный `d` | RESUME (и исправить клиент) |
| 4003 | нет IDENTIFY/RESUME за 10 с | IDENTIFY |
| 4004 | тикет неверный или использован, сессия входа отозвана (выход, кража refresh) | новый тикет и IDENTIFY; если тикет не выдают (401), войти заново |
| 4005 | повторный IDENTIFY/RESUME в том же соединении | RESUME |
| 4006 | сессию нельзя продолжить: неизвестна, истекла, `seq` вне буфера, чужая | IDENTIFY |
| 4008 | больше 120 кадров в минуту | RESUME после паузы |
| 4009 | пропущен heartbeat | RESUME |
| 4010 | клиент не успевает читать | RESUME |
| 4012 | неверная версия `v` | не переподключаться |
| 4013 | 20 подключённых сессий пользователя на ноде (оборванные при этом вытесняются, начиная с самой старой) | не переподключаться автоматически |
| 1012 | сервер перезапускается | RESUME (скорее всего 4006 → IDENTIFY) |
| 1000, 1001 | закрыл клиент | сессия завершена |
| 1009 | кадр больше 4 КиБ | RESUME (и исправить клиент) |

События и кому они приходят:

| События | Получатель |
|---|---|
| `GUILD_CREATE` | вам при создании сервера или входе; собран из текущего состояния, без скрытых каналов |
| `GUILD_UPDATE`, `GUILD_DELETE` | участникам |
| `CHANNEL_CREATE/UPDATE/DELETE` сервера | тем, кто видит канал. Если изменение ролей, переопределений или владельца открыло канал, приходит `CHANNEL_CREATE`, если скрыло — `CHANNEL_DELETE {id, guild_id}` |
| `GUILD_ROLE_*`, `GUILD_MEMBER_*` | участникам |
| `GUILD_PERMISSIONS_UPDATE {guild_id, permissions, channel_permissions}` | вам, когда изменились ваши права на сервере (роли, переопределения, своё участие, владелец, новый или удалённый видимый канал). Полностью заменяет оба поля; приходит после событий, которые его вызвали |
| `INVITE_CREATE/DELETE` | участникам с `MANAGE_GUILD` |
| `MESSAGE_*` в канале сервера | тем, кто видит канал (`VIEW_CHANNEL`) |
| `MESSAGE_*` в личке, `CHANNEL_*` личек, `CHANNEL_RECIPIENT_*` | участникам лички |
| `RELATIONSHIP_*` | вам |
| `USER_UPDATE` | вашим сессиям, когда изменился ваш аккаунт (имя, «о себе», аватар, выбранный статус); полный пользователь |
| `VOICE_STATE_UPDATE` | участникам сервера, кто видит голосовой канал: вошёл, вышел, выключил микрофон или звук; голосовое состояние, `channel_id: null` — вышел (так же выглядит переход в скрытый от вас канал). Когда канал становится видимым, после `CHANNEL_CREATE` приходят состояния тех, кто в нём |
| `VOICE_MOVE {guild_id, channel_id, from_channel_id}` | вашим сессиям, когда модератор перенёс вас в другой голосовой канал: клиент, который был в `from_channel_id`, входит в `channel_id` (`POST /channels/{id}/voice`). Из старой комнаты ядро выводит сразу (ADR-0031) |
| `CALL_CREATE`, `CALL_UPDATE` | получателям лички: звонок `{id, channel_id, started_by, started_at, ringing, ring_until, members}`. Звонить перестают в `ring_until`, отдельного события нет (ADR-0032) |
| `CALL_DELETE {id, channel_id}` | получателям лички: звонок закончился |
| `SESSION_CREATE` | вашим другим сессиям, когда вы вошли на новом устройстве; сессия без `current` |
| `SESSION_REVOKE` | вашим другим сессиям, когда одна из ваших сессий завершена; `{session_id}`. Сокеты самой завершённой сессии закрываются с кодом 4004 |
| `USER_PROFILE_UPDATE {user}` | участникам ваших серверов, когда изменились имя или аватар; публичный пользователь. Одно изменение приходит один раз, сколько бы общих серверов ни было |
| `TYPING_START` | тем, кто видит канал; кроме вас самих |
| `PRESENCE_UPDATE {user_id, status}` | друзьям и участникам общих серверов до 250 человек; одно изменение приходит один раз, даже если вы делите несколько серверов |

Клиент обрабатывает `*_CREATE` и `*_UPDATE` как вставку или замену по id:
после IDENTIFY личные события (связи, лички, `GUILD_CREATE`) и сообщения,
случившиеся во время загрузки READY, могут прийти ещё раз. Изменения серверов,
уже вошедшие в READY, повторно не приходят. Свой `GUILD_MEMBER_ADD` при входе на сервер не приходит: вы уже есть в
`GUILD_CREATE`. Права никогда не пересчитываются на клиенте: актуальные
значения приходят в `GUILD_PERMISSIONS_UPDATE`.

### Статусы

- Устройство сообщает, используется ли оно: `{op: 3, d: {status: "online" | "idle"}}`
  (`idle` — после бездействия, `online` — при возвращении). Другие значения
  закрывают соединение кодом 4002, до IDENTIFY — 4003. По умолчанию после
  IDENTIFY устройство `online`.
- Видимый другим статус: `offline`, если нет ни одной сессии или выбран
  `invisible`; иначе выбранный `dnd` или `idle`; иначе `online`, если хоть одно
  устройство в работе, и `idle`, если все бездействуют.
- После обрыва сети статус держится окно RESUME (2 минуты), чтобы
  переподключение не мигало у друзей. Закрытие вкладки (код 1000/1001)
  показывает `offline` сразу. При перезапуске сервера (1012) статус не
  сбрасывается.
- `TYPING_START` и `PRESENCE_UPDATE` не хранятся и не повторяются при RESUME:
  пропущенный статус восстановится при следующем изменении или в READY,
  статусы участников сервера — в `GET /guilds/{id}/members`. Новому другу его
  текущий статус приходит сразу после `RELATIONSHIP_ADD`, если он не `offline`.

## Поиск: ограничение времени (аудит S5)

Поиск, который идёт дольше 3 с, прерывается: 422 `search_too_slow`
«Поиск занял слишком много времени: добавьте слова или выберите канал».
