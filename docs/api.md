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
