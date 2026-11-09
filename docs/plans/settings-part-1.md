# Настройки, часть 1: план реализации

> **Для исполнителя:** ОБЯЗАТЕЛЬНЫЙ НАВЫК: superpowers:subagent-driven-development или superpowers:executing-plans. Шаги отмечаются чекбоксами (`- [ ]`).

**Цель:** перенести голос и камеру в настройки, дать проверку устройств вне звонка и новое меню.

**Архитектура:** настройки звука — чистый модуль с хранением в браузере, `VoiceService` применяет их к комнате. Проверка микрофона — отдельный сервис без LiveKit. Разделы — новые компоненты в `widgets/user`, общая раскладка «настройки + живая колонка» в `SettingsSplit`.

**Стек:** Angular 21 (standalone, OnPush, signals), Tailwind 4 с токенами `ok-`, livekit-client 2.23.0, Karma/Jasmine, Playwright.

**Спецификация:** `ruscord-core/docs/plans/settings.md`, раздел «Часть 1». Части 2 и 3 — отдельные планы после выпуска этой.

## Общие требования

- Весь код в `ruscord-fe`, ветка `settings-voice`. Документация — в `ruscord-core`, ветка `settings-voice`.
- Без `Co-Authored-By` и ссылок на сессии в коммитах, без футера «Generated with» в PR (CLAUDE.md ядра).
- Комментарии в коде на английском, тексты интерфейса на русском.
- `livekit-client` только в `VoiceService`. Стили компонента до 8 кБ.
- Цвета только токенами `ok-`, активный пункт меню — `bg-ok-accent text-ok-on-accent`, скругления `rounded-ok-md` и `rounded-ok-lg`.
- Микрофон и камера включаются только нажатием кнопки.
- В меню нет заглушек.
- Перед началом: `npm ci` в `ruscord-fe` (в локальной копии нет `node_modules`).
- Проверка одного файла: `npm run test:ci -- --include=<путь к spec>`.

## На что смотреть при проверке

1. Быстрые нажатия «Слышать себя»: устаревший `getUserMedia` должен освободить микрофон (задача 3).
2. Уход из вкладки при удержании клавиши рации: микрофон не должен остаться открытым (задача 4).
3. Испорченные или заблокированные настройки в браузере: значения по умолчанию, без падения (задача 1).
4. Вход в звонок и закрытие настроек во время проверки: устройство освобождено (задача 6).
5. Сохранённое устройство отключили: список показывает «По умолчанию» (задача 6).

---

### Задача 1: настройки звука

**Файлы:**
- Создать: `src/entities/voice/model/voice-prefs.ts`, `src/entities/voice/model/voice-prefs.spec.ts`
- Изменить: `src/entities/voice/index.ts`

**Интерфейсы:**
- Создаёт:
  ```ts
  export type ProcessingMode = 'suppress' | 'clean' | 'custom';
  export interface AudioFlags { noiseSuppression: boolean; echoCancellation: boolean; autoGainControl: boolean }
  export interface VoicePrefs { mode: ProcessingMode; custom: AudioFlags; volume: number; talkMode: boolean; talkKey: string }
  export const DEFAULT_VOICE_PREFS: VoicePrefs; // suppress, все custom true, volume 1, talkMode false, talkKey 'Backquote'
  export function flagsFor(p: Pick<VoicePrefs, 'mode' | 'custom'>): AudioFlags;
  export function loadVoicePrefs(): VoicePrefs;
  export function saveVoicePrefs(p: VoicePrefs): void;
  export function talkKeyAllowed(code: string): boolean; // запрещены Escape, Tab, Enter, Space, пустая строка
  export function talkKeyLabel(code: string): string;    // Backquote→'Ё', KeyX→'X', Digit1→'1', F5→'F5', иначе code
  ```
  Ключ хранения: `ruscord.voice.prefs` (JSON).

- [ ] **Шаг 1: написать падающие тесты** в `voice-prefs.spec.ts`:
  - `flagsFor({mode:'suppress',custom:ВСЕ_FALSE})` → все `true`; `clean` → все `false`; `custom` возвращает `custom`.
  - `loadVoicePrefs` без записи равен `DEFAULT_VOICE_PREFS`.
  - запись `'{broken'` → значения по умолчанию.
  - `{"mode":"loud","volume":7,"talkKey":5}` → `mode:'suppress'`, `volume:1`, `talkKey:'Backquote'`.
  - `volume:-1` → `0`; отсутствующее поле `custom.echoCancellation` → `true`.
  - `localStorage.getItem` и `setItem` бросают (spyOn) → загрузка отдаёт значения по умолчанию, сохранение не падает.
  - `talkKeyAllowed`: `'Escape'`, `'Tab'`, `'Enter'`, `'Space'`, `''` → false; `'KeyV'` → true.
  - `talkKeyLabel('Backquote')==='Ё'`, `('KeyV')==='V'`, `('Digit3')==='3'`, `('F2')==='F2'`.
- [ ] **Шаг 2: запустить, убедиться, что падают** (`--include=src/entities/voice/model/voice-prefs.spec.ts`).
- [ ] **Шаг 3: реализовать** модуль и экспорт из `index.ts`. `try/catch` вокруг хранилища, как `readDevice`.
- [ ] **Шаг 4: запустить тесты**, ожидается PASS.
- [ ] **Шаг 5: коммит** `voice: remembered audio processing, volume and talk key`.

### Задача 2: применение в `VoiceService`

**Файлы:**
- Изменить: `src/entities/voice/model/voice.service.ts`, `src/entities/voice/model/voice.service.spec.ts`, `src/entities/voice/index.ts`

**Интерфейсы:**
- Использует: `VoicePrefs`, `AudioFlags`, `flagsFor`, `loadVoicePrefs`, `saveVoicePrefs`.
- Создаёт:
  ```ts
  export function micWanted(s: { canSpeak: boolean; selfMute: boolean; selfDeaf: boolean; talkRequired: boolean; talkHeld: boolean }): boolean;
  // в VoiceService:
  readonly prefs: WritableSignal<VoicePrefs>;
  readonly flags: Signal<AudioFlags>;
  readonly talkRequired: Signal<boolean>; // pushToTalk() || prefs().talkMode
  updatePrefs(patch: Partial<VoicePrefs>): Promise<void>;
  holdTalk(down: boolean): Promise<void>;
  ```

- [ ] **Шаг 1: написать падающие тесты.**
  - Таблица `micWanted`: без рации и без mute → true; рация без удержания → false; рация с удержанием → true; рация с удержанием и `selfMute` → false; и с `selfDeaf` → false; `canSpeak:false` → false.
  - С `TestBed` и заглушками `ApiService`, `GatewayService {dispatches: new Subject()}`, `AuthService {signedIn: signal(true), user: signal(null)}`:
    - `updatePrefs({volume:.4})` ставит `volume===.4` элементам из приватной `audio` и сохраняет настройки.
    - смена `mode` при `room` с дорожкой микрофона вызывает `restartTrack({deviceId:undefined, noiseSuppression:false, echoCancellation:false, autoGainControl:false})` для `clean`.
    - смена `volume` не вызывает `restartTrack`.
- [ ] **Шаг 2: запустить, убедиться, что падают.**
- [ ] **Шаг 3: реализовать.** После `npm ci` свериться с `LocalAudioTrack.restartTrack` в `node_modules/livekit-client/dist/src/room/track/LocalAudioTrack.d.ts`.
  - `audioCaptureDefaults`: `{...this.flags(), deviceId: this.input() || undefined}`.
  - Приватный `syncMic()` = `setMic(micWanted(...))`. Заменить им все вызовы `setMic(...)` в `join`, `toggleMute`, `toggleDeaf`, `permissionsChanged`.
  - `attach`: `el.volume = this.prefs().volume`.
  - `updatePrefs`: обновить и сохранить; при смене `flags` перезапустить дорожку `Track.Source.Microphone`, если она есть; при смене `volume` пройти по `audio`; при смене `talkRequired` вызвать `syncMic()`. Ошибку перезапуска показать в `error`: «Не удалось применить настройки микрофона».
  - `holdTalk`: выходит, если `talkHeld` не изменился или `!talkRequired()`, иначе `syncMic()`.
  - `reset`: `pushToTalk.set(false)`, `talkHeld = false`.
  - Убрать комментарий «Push-to-talk comes with the voice settings».
- [ ] **Шаг 4: запустить тесты** `voice.service.spec.ts`, ожидается PASS.
- [ ] **Шаг 5: коммит** `voice: apply processing, volume and push-to-talk`.

### Задача 3: проверка микрофона и сигнал

**Файлы:**
- Создать: `src/entities/voice/model/mic-check.ts`, `src/entities/voice/model/mic-check.spec.ts`
- Изменить: `src/entities/voice/index.ts`

**Интерфейсы:**
- Использует: `AudioFlags`.
- Создаёт:
  ```ts
  export function levelOf(samples: Float32Array): number; // RMS × 4, в пределах 0..1
  export function micErrorText(err: unknown): string;
  export interface MicCheckOptions { input: string; output: string; flags: AudioFlags }
  @Injectable({ providedIn: 'root' })
  export class MicCheck {
    readonly running: Signal<boolean>;
    readonly level: Signal<number>;
    readonly error: Signal<string | null>;
    start(o: MicCheckOptions): Promise<void>;
    stop(): void;
    playTone(output: string): Promise<void>;
  }
  ```

- [ ] **Шаг 1: написать падающие тесты.** `getUserMedia` — spyOn, отдаёт `MediaStream` из `AudioContext.createMediaStreamDestination()`:
  - `levelOf(нули)===0`; `levelOf(единицы)===1`; амплитуда `0.1` → примерно `0.4`.
  - `micErrorText`: `NotAllowedError` → содержит «разрешите»; `NotFoundError` → содержит «не найден»; `NotReadableError` → содержит «занят».
  - `start` запрашивает `{audio:{deviceId:'a',noiseSuppression:true,echoCancellation:true,autoGainControl:true}}`, `running` true; при пустом `input` `deviceId` — `undefined`.
  - `stop` вызывает `stop()` у всех дорожек, `running` false, `level` 0.
  - **Устаревший запуск:** первый `getUserMedia` не завершён, вызваны `stop()` и второй `start`; когда первый завершается, его дорожки остановлены, `running` true от второго.
  - Отказ в доступе: `error` заполнен, `running` false.
  - `start` при идущей проверке останавливает прежние дорожки.
- [ ] **Шаг 2: запустить, убедиться, что падают.**
- [ ] **Шаг 3: реализовать.** Уровень — `AnalyserNode` (`fftSize` 1024, `getFloatTimeDomainData`) в `requestAnimationFrame`. Голос — `<audio>` с `srcObject` и `setSinkId(output)`, если выбран и метод есть. Устаревание — счётчик запусков. `playTone`: 440 Гц, 400 мс, громкость 0.15, через `MediaStreamDestination` и `<audio>` с тем же `setSinkId`.
- [ ] **Шаг 4: запустить тесты**, ожидается PASS.
- [ ] **Шаг 5: коммит** `voice: check the microphone and speakers without a call`.

### Задача 4: клавиша рации

**Файлы:**
- Изменить: `src/features/hotkeys/lib/keys.ts`, `src/features/hotkeys/model/hotkeys.service.ts`
- Тест: `src/features/hotkeys/lib/keys.spec.ts` (добавить)

**Интерфейсы:**
- Использует: `VoiceService.prefs`, `talkRequired`, `holdTalk`.
- Создаёт:
  ```ts
  export function isTyping(target: EventTarget | null): boolean; // INPUT, TEXTAREA, SELECT, contenteditable
  export function startsTalk(e: { code: string; repeat: boolean }, c: { key: string; required: boolean; typing: boolean; modalOpen: boolean }): boolean;
  ```

- [ ] **Шаг 1: написать падающие тесты:** `startsTalk` истинно при совпадении `code` и `required`; ложно при `repeat`, `typing`, `modalOpen`, чужом `code`, `required:false`. `isTyping(document.createElement('textarea'))` true; `('input')` true; `('div')` false; div с `contentEditable='true'` true; `null` false.
- [ ] **Шаг 2: запустить, убедиться, что падают.**
- [ ] **Шаг 3: реализовать** функции и в `start()` слушатели: `keydown` → `startsTalk` → `holdTalk(true)`; `keyup` с той же клавишей → `holdTalk(false)`; `window` `blur` → `holdTalk(false)`. Отпускание срабатывает всегда, без проверки фокуса.
- [ ] **Шаг 4: запустить тесты**, ожидается PASS.
- [ ] **Шаг 5: коммит** `voice: hold a key to talk`.

### Задача 5: меню и раздел «Вход и сеансы»

**Файлы:**
- Создать: `src/widgets/user/ui/settings-sections.ts`, `settings-sections.spec.ts`, `src/widgets/user/ui/settings-split.ts`
- Изменить: `src/widgets/user/ui/UserSettingsModal.ts`, `src/entities/app-state/model/app-state.store.ts:61`, `e2e/smoke.mjs:691,754`

**Интерфейсы:**
- Создаёт:
  ```ts
  export type SettingsSection = 'profile' | 'login' | 'voice' | 'camera' | 'connection';
  export const SETTINGS_GROUPS: readonly { label: string; items: readonly { key: SettingsSection; label: string }[] }[];
  export function sectionFrom(value: unknown): SettingsSection;
  ```
  Компонент `SettingsSplit`: основное содержимое через `<ng-content />`, колонка — `<ng-content select="[preview]" />`; `flex flex-wrap`, колонка от 280 до 320 пикселей, на узком экране уходит вниз.

- [ ] **Шаг 1: написать падающие тесты:** группы и подписи точно как в таблице спецификации (`АККАУНТ`: Профиль, Вход и сеансы; `ЗВОНКИ`: Голос, Камера, Соединение); `sectionFrom('voice')==='voice'`; `sectionFrom('security')==='profile'`; `sectionFrom(undefined)==='profile'`.
- [ ] **Шаг 2: запустить, убедиться, что падают.**
- [ ] **Шаг 3: реализовать.** Модальное окно строит меню по `SETTINGS_GROUPS`; ширина содержимого `max-w-[1080px]` для `voice` и `camera`, иначе прежние 760px. Содержимое `voice`, `camera` и `connection` добавят задачи 6 и 7, поэтому **задачи 5–8 выпускаются одним PR**. Сообщение входа: `{ section: 'login' }`, подпись «Открыть сеансы». В `e2e/smoke.mjs` строки 691 и 754 — на «Вход и сеансы».
- [ ] **Шаг 4: запустить тесты и сборку** `npm run build`, ожидается успех.
- [ ] **Шаг 5: коммит** `settings: new menu; login and sessions`.

### Задача 6: раздел «Голос»

**Файлы:**
- Создать: `src/widgets/user/ui/sections/voice-section.ts`, `voice-section.spec.ts`
- Изменить: `src/widgets/user/ui/UserSettingsModal.ts`

**Интерфейсы:**
- Использует: `VoiceService`, `MicCheck`, `talkKeyAllowed`, `talkKeyLabel`, `SettingsSplit`, `Dolobeb`.
- Создаёт:
  ```ts
  export function checkFace(running: boolean, level: number): DolobebState; // !running→idle; level>0.08→speaking; иначе listening
  export function selectedOr(list: readonly { deviceId: string }[], id: string): string; // id, если есть в списке, иначе ''
  ```
  Селекторы: `data-settings-voice`, `data-mic-check`, `data-mic-level`, `data-speaker-check`, `data-processing="<mode>"`, `data-volume`, `data-talk-toggle`, `data-talk-key`.

- [ ] **Шаг 1: написать падающие тесты:**
  - `checkFace` на всех трёх ветках, порог 0.08 не включается.
  - `selectedOr` с отключённым устройством → `''`.
  - С компонентом и заглушками: `voice.connected` true → кнопка `[data-mic-check]` отключена, виден текст «Завершите звонок, чтобы проверить микрофон»; проверка шла и `connected` стал true → вызван `stop`.
  - Уничтожение компонента вызывает `stop`.
  - При идущей проверке смена `voice.input` вызывает повторный `start` с новым устройством.
  - Режим `custom` показывает три переключателя, `suppress` — нет.
  - Назначение `Escape` клавишей рации не меняет `talkKey` и показывает подсказку.
- [ ] **Шаг 2: запустить, убедиться, что падают.**
- [ ] **Шаг 3: реализовать** по спецификации и макету `settings-a-screens.html`. Подписи режимов и «Как вас слышно» — дословно из спецификации. Списки — `selectedOr`; при открытии и на `devicechange` — `refreshDevices`; после `running` первый раз — снова `refreshDevices` (появились названия). Нет `outputs` — подсказка «Этот браузер не даёт выбрать динамики.». Подпись рации: «Микрофон работает, пока удерживаете клавишу. В браузере только при открытой вкладке.».
- [ ] **Шаг 4: запустить тесты**, ожидается PASS.
- [ ] **Шаг 5: коммит** `settings: voice section`.

### Задача 7: разделы «Камера» и «Соединение»

**Файлы:**
- Создать: `src/widgets/user/ui/sections/camera-section.ts`, `camera-section.spec.ts`, `connection-section.ts`
- Изменить: `src/widgets/user/ui/UserSettingsModal.ts`

**Интерфейсы:**
- Использует: `VoiceService`, `cameraErrorText`, `VideoView` (`[source]`, `[userId]`), `VoiceStats`, `Dolobeb`, `AuthService.user`, `SettingsSplit`.
- Селекторы: `data-settings-camera`, `data-camera-preview`, `data-camera-toggle`, `data-settings-connection`.

- [ ] **Шаг 1: написать падающие тесты** (`getUserMedia` — spyOn):
  - при открытии раздела `getUserMedia` не вызывается;
  - кнопка запрашивает `{video:{deviceId}}`, повторное нажатие останавливает дорожки;
  - уничтожение компонента останавливает дорожки;
  - отключение во время незавершённого запроса: дорожки поздно пришедшего потока остановлены;
  - `NotAllowedError` показывает текст `cameraErrorText`;
  - смена камеры при включённом предпросмотре запускает его заново;
  - `voice.cameraOn()` true → показан `VideoView`, собственный запрос не выполняется.
- [ ] **Шаг 2: запустить, убедиться, что падают.**
- [ ] **Шаг 3: реализовать.** Видео зеркально (`-scale-x-100`), `autoplay playsinline muted`. «Соединение»: `voice.connected()` → `VoiceStats`, иначе `Dolobeb state="sleeping"` и «Статистика появится во время звонка».
- [ ] **Шаг 4: запустить тесты и сборку**, ожидается успех.
- [ ] **Шаг 5: коммит** `settings: camera and connection sections`.

### Задача 8: панель звонка, e2e, документация

**Файлы:**
- Изменить: `src/widgets/voice/ui/voice-panel.ts`, `e2e/smoke.mjs:559-565`
- Ядро (ветка `settings-voice`): `docs/devlog.md`, `docs/product/screens.md:33`, `docs/plans/settings.md`

**Интерфейсы:**
- Использует: `ModalService.open('userSettings', {section:'voice'})`, `talkKeyLabel`, `VoiceService.prefs`.

- [ ] **Шаг 1: переписать e2e (падает на старой панели).** Новый шаг smoke вне звонка: открыть настройки → «Голос»; `[data-mic-check]`; дождаться `Number(getAttribute('data-mic-level')) > 0`; уйти на «Камера», `[data-camera-toggle]`, `[data-camera-preview] video` с `videoWidth > 0`; закрыть. В строках 559–565: `button[title="Настройки голоса и видео"]`, «Соединение», `[data-voice-stats]` с «Камера:», закрыть настройки (`Escape`). Рация: Bob в голосе включает `[data-talk-toggle]`, закрывает настройки, Ann видит тишину (нет `data-speaking` у Bob 3 с); `bob.keyboard.down('Backquote')` → `[data-voice-tile="BOB"][data-speaking]` у Ann; `up`, затем отключить рацию.
- [ ] **Шаг 2: убрать из `VoicePanel`** списки, `VoiceStats`, `devicesOpen`, `statsOpen`, `toggleDevices`, `pick`; шестерёнка — `title="Настройки голоса и видео"`, открывает раздел `voice`. Подсказка при `voice.pushToTalk()`: «На этом канале только рация: удерживайте клавишу {{ label }}».
- [ ] **Шаг 3: запустить всё.** `npm run test:ci`, `npm run build`, `e2e/ci.sh ../ruscord-core` (нужен Docker). Ожидается: все зелёные.
- [ ] **Шаг 4: коммит клиента** `voice: the call panel opens the settings`.
- [ ] **Шаг 5: документация в ядре:** запись в devlog (сделано, узнали, открыто); в `screens.md` строку про раздел «Голос и видео» заменить на сделанное; статус `settings.md` — «принято», добавить таблицу «Ход работ». Коммит `docs: settings, part 1`.
