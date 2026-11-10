# Настройки, часть 2: настроение и цвет профиля в ядре — план реализации

> **Для исполнителя:** ОБЯЗАТЕЛЬНЫЙ НАВЫК: superpowers:subagent-driven-development или superpowers:executing-plans. Шаги отмечаются чекбоксами (`- [ ]`).

**Цель:** ядро хранит и отдаёт настроение (лицо долобеба, фраза, срок) и цвет профиля, другие узнают о смене сразу.

**Архитектура:** четыре колонки в `users` (модуль `auth`), разбор `mood` в `PATCH /users/@me` с различием «нет поля» и `null`, новые поля в `User` и `PublicUser`. Истёкшее настроение отсекается при чтении, без фоновых задач. Событие `USER_PROFILE_UPDATE` уходит через существующий `change`.

**Стек:** Go 1.26, pgx v5, goose, Postgres 18; тесты — `apitest` с настоящим Postgres.

**Спецификация:** `docs/plans/settings.md`, раздел «Часть 2» → «Ядро». Клиент (редактор и карточка) — часть 3, отдельный план.

## Общие требования

- Ветка `settings-mood` в ruscord-core от `settings-voice` (там лежит спецификация). Без push.
- Правила CLAUDE.md: тесты на новую логику; протокол gateway без табличного теста не мержится; запись в `devlog.md`; новое решение — ADR-0046; непроверенное допущение — в `assumptions.md`. В коммитах нет `Co-Authored-By` и ссылок на сессии.
- Комментарии в коде на английском, тексты ошибок и документация на русском.
- Модули общаются через интерфейсы: таблицу `users` читает только `auth`.
- Состояния настроения: `happy`, `laughing`, `thinking`, `sleeping`, `listening`, `surprised`, `confused`, `angry`.
- Цвета профиля: `accent`, `success`, `warning`, `danger`, `neutral`; по умолчанию `accent`.
- Текст настроения: до 80 символов, необязателен.
- Срок: в будущем и не дальше 7 дней.
- Go локально нет. Все тесты и линтер: `make test-docker` (лог в `test-docker.log`). Для одного теста прочитать `scripts/test-docker.sh` и запустить тот же образ с `-run`.

## На что смотреть при проверке

1. Часы клиента спешат на минуту: срок «7 дней» не должен отклоняться (задача 2, допуск 5 минут).
2. `PATCH` только с `bio`: настроение и цвет не сбрасываются (задача 2).
3. Удалённый аккаунт: в его публичном профиле нет настроения, а цвет `accent` (задача 3).
4. Настроение истекло между записью и чтением: в списках участников и профиле его нет (задача 3).
5. Повтор того же настроения не рассылает событие гильдиям (задача 3).

---

### Задача 1: миграция и поля пользователя

**Файлы:**
- Создать: `migrations/00022_mood.sql`
- Изменить: `internal/auth/store.go` (`User`, `userCols`, `scanUser`), `internal/auth/lookup.go` (`PublicUser`, `Public`, `publicCols`, `scanPublic`)
- Создать: `internal/auth/mood.go`; тест: `internal/auth/unit_test.go` (дополнить)

**Интерфейсы:**
- Создаёт:
  ```go
  // mood.go
  type Mood struct {
      State     string     `json:"state"`
      Text      string     `json:"text"`
      ExpiresAt *time.Time `json:"expires_at"`
  }
  var MoodStates = []string{"happy", "laughing", "thinking", "sleeping", "listening", "surprised", "confused", "angry"}
  var ProfileColors = []string{"accent", "success", "warning", "danger", "neutral"}
  func liveMood(state *string, text string, expires *time.Time, now time.Time) *Mood // nil without state or when expires <= now
  ```
  `User` и `PublicUser` получают `Mood *Mood \`json:"mood,omitempty"\`` и `ProfileColor string \`json:"profile_color"\``.
- Миграция: `mood_state text CHECK (mood_state IN (…8…))`, `mood_text text NOT NULL DEFAULT '' CHECK (char_length(mood_text) <= 80)`, `mood_expires_at timestamptz`, `profile_color text NOT NULL DEFAULT 'accent' CHECK (profile_color IN (…5…))`, и `CHECK (mood_state IS NOT NULL OR (mood_text = '' AND mood_expires_at IS NULL))`. Down удаляет колонки.

- [ ] **Шаг 1: написать падающий тест** `TestLiveMood` (табличный): без состояния → nil; состояние без срока → `Mood` с текстом; срок в будущем → `Mood`; срок равен `now` → nil; срок в прошлом → nil.
- [ ] **Шаг 2: запустить, убедиться, что падает** (не компилируется).
- [ ] **Шаг 3: реализовать** миграцию, `mood.go`, колонки в `userCols`/`publicCols` и разбор в `scanUser`/`scanPublic` через `liveMood(…, time.Now())`. `Public()` копирует `Mood` и `ProfileColor`. Для удалённого аккаунта `scanPublic` ставит `Mood = nil`, `ProfileColor = "accent"`.
- [ ] **Шаг 4: запустить** `make test-docker`, ожидается PASS (существующие тесты не ломаются от новых полей).
- [ ] **Шаг 5: коммит** `auth: mood and profile colour columns`.

### Задача 2: `PATCH /users/@me` принимает настроение и цвет

**Файлы:**
- Изменить: `internal/auth/profile.go` (`UpdateMeInput`, `UpdateMe`, `current`, `change`), `internal/auth/account.go:61`
- Тест: `internal/auth/profile_test.go`

**Интерфейсы:**
- Использует: `Mood`, `MoodStates`, `ProfileColors` (задача 1).
- Создаёт:
  ```go
  // Optional tells an absent JSON field from an explicit null.
  type Optional[T any] struct { Set bool; Value *T }
  func (o *Optional[T]) UnmarshalJSON(b []byte) error
  type MoodInput struct {
      State     string     `json:"state"`
      Text      string     `json:"text"`
      ExpiresAt *time.Time `json:"expires_at"`
  }
  // UpdateMeInput gains:
  Mood         Optional[MoodInput] `json:"mood"`
  ProfileColor *string             `json:"profile_color"`
  ```
  `Optional` кладётся в `internal/platform/httpx`, если там нет похожего (сначала поискать).

- [ ] **Шаг 1: написать падающие тесты** в `profile_test.go`.
  - `TestUpdateMoodValidation`, таблица кодов и поля ошибки:
    - `{"mood":{"state":"happy"}}` → 200;
    - неизвестное состояние `"bored"` → 422 `mood`;
    - текст 81 символ (`я`) → 422 `mood`; текст 80 → 200 и обрезка пробелов по краям;
    - NUL в тексте → 422 `mood`;
    - срок в прошлом → 422 `mood`; срок `now+7d+1m` → 200 (допуск); `now+7d+10m` → 422;
    - `{"mood":{"text":"x"}}` без состояния → 422 `mood`;
    - лишнее поле внутри `mood` → 400;
    - `profile_color: "pink"` → 422 `profile_color`; `"neutral"` → 200.
  - `TestMoodSetKeepClear`: поставить настроение и цвет `success`; `PATCH {"bio":"x"}` оставляет оба; `{"mood":null}` очищает настроение, цвет остаётся; пустое тело → 422 с сообщением, где перечислены `mood` и `profile_color`.
  - `TestDeleteAccount` (в `account_test.go` дополнить): после удаления в строке `mood_state IS NULL`, `mood_text = ''`, `mood_expires_at IS NULL`.
- [ ] **Шаг 2: запустить, убедиться, что падают.**
- [ ] **Шаг 3: реализовать.**
  - Проверки через `httpx.Fields` (длина текста по символам, NUL).
  - Срок: `> s.now()` и `<= s.now()+7×24h+5m`. Ошибка: «Срок — в будущем и не дальше 7 дней».
  - Состояние: «Одно из: happy, laughing, …». Цвет: «Одно из: accent, success, warning, danger, neutral».
  - `UPDATE` пишет настроение только при `Mood.Set`: при `null` — `NULL`, `''`, `NULL`.
  - `current` читает `mood_state`, `mood_text`, `mood_expires_at`, `profile_color`. `public` истинно, если отличается имя, настроение (все три поля) или цвет.
  - `errNoChanges`: «Укажите хотя бы одно поле: display_name, bio, status, mood, profile_color».
  - Удаление аккаунта (`account.go:61`) в том же `UPDATE` очищает три поля настроения.
- [ ] **Шаг 4: запустить** `make test-docker`, ожидается PASS.
- [ ] **Шаг 5: коммит** `auth: set and clear the mood and the profile colour`.

### Задача 3: что видят другие

**Файлы:**
- Тест: `internal/auth/profile_test.go`, `internal/gateway/router_test.go:434-453`

**Интерфейсы:**
- Использует: всё из задач 1 и 2.

- [ ] **Шаг 1: написать падающие тесты.**
  - `TestMoodFanOut`, по образцу `TestProfileFanOut`:
    - смена настроения пишет `USER_PROFILE_UPDATE` в каждую гильдию, и `user.mood.state` в событии совпадает;
    - тот же запрос повторно событий гильдиям не пишет, только `USER_UPDATE`;
    - смена цвета пишет событие с `profile_color`.
  - `TestExpiredMoodHidden`: поставить настроение, затем `UPDATE users SET mood_expires_at = now() - interval '1 second'` напрямую в БД. В `GET /users/@me`, в профиле другого пользователя и в списке участников гильдии поля `mood` нет.
  - `TestDeletedUserHasNoMood`: удалить аккаунт с настроением; публичный профиль без `mood`, `profile_color = "accent"`.
  - В табличном тесте `router_test.go` строка: тот же пользователь, то же имя, другое настроение → доставляется (`1`).
- [ ] **Шаг 2: запустить, убедиться, что падают** (или зафиксировать, какие проходят уже после задач 1–2, и почему).
- [ ] **Шаг 3: исправить**, что не проходит. `gateway/router.go` сравнивает событие целиком, поэтому изменений там, скорее всего, не нужно; если нужно — описать в отчёте.
- [ ] **Шаг 4: запустить** `make test-docker` (тесты и линтер), ожидается PASS.
- [ ] **Шаг 5: коммит** `auth: others see the mood and the colour; expired and deleted hide it`.

### Задача 4: документация

**Файлы:**
- Создать: `docs/adr/0046-mood.md` (по шаблону `0000-template.md`)
- Изменить: `docs/api.md:42,73-80`, `docs/plans/discord-parity.md:39-40`, `docs/assumptions.md`, `docs/devlog.md`, `docs/plans/settings.md` (таблица «Ход работ»)

- [ ] **Шаг 1:** ADR-0046:
  - настроение вместо текстового статуса;
  - 8 состояний долобеба, цвет — имя токена;
  - срок отсекается при чтении без события об истечении;
  - допуск 5 минут на часы клиента;
  - пересмотреть, если понадобится событие об истечении (например, для ботов).
- [ ] **Шаг 2:** `api.md`:
  - строка `PATCH /users/@me` с `mood?`, `profile_color?`;
  - абзац про `mood` (`null` очищает, отсутствие не меняет), значения, срок;
  - `mood` и `profile_color` в объекте пользователя.
- [ ] **Шаг 3:** `discord-parity.md`:
  - «Свой статус» — API (настроение), ADR-0046;
  - «Баннер, цвет профиля» — цвет API, баннер нет.
- [ ] **Шаг 4:** `assumptions.md`: новое допущение «часы клиентов расходятся с сервером не больше чем на 5 минут» с номером по порядку.
- [ ] **Шаг 5:** devlog (сделано, узнали, открыто — клиент в части 3); «Ход работ» в `settings.md` — строка части 2.
- [ ] **Шаг 6:** коммит `docs: mood and profile colour (ADR-0046)`.
