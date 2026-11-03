# Тестовый сервер на VPS

Как поднять ruscord на одном VPS для проверки с друзьями: сайт по HTTPS,
голос, видео и показ экрана. Почему устроено так — ADR-0033.

## Что получится

```
браузер ──HTTPS 443──▶ Caddy ──▶ nginx клиента ──▶ ядро (/api, /gateway, /cdn)
                                              └──▶ LiveKit (/livekit, сигналинг)
браузер ──UDP 7882 / TCP 7881──▶ LiveKit (звук, видео, экран)
```

- Снаружи открыты только Caddy (80, 443) и медиапорты LiveKit (7882/udp,
  7881/tcp).
- Postgres, Valkey, NATS, SeaweedFS, ядро и метрики слушают только
  127.0.0.1 или внутреннюю сеть Docker.
- Сертификат Let's Encrypt Caddy получает и продлевает сам.

## Что нужно заранее

- **VPS** с публичным IPv4 и Ubuntu 24.04 (или другой Linux с Docker).
  - На сервере собираются ядро (Go) и клиент (Angular). Для сборки нужно
    около 4 ГБ памяти; с 2 ГБ — добавьте swap (ниже).
  - Сколько нужно на звонки, замерим на проверке. Ориентир для боевой
    нагрузки — docs/architecture.md, «LiveKit в проде».
- **Домен** (или поддомен), A-запись которого указывает на IP сервера.
- **Доступ по SSH** с правами sudo.

Проверьте DNS до установки: `dig +short ваш.домен` должен вернуть IP
сервера. Без этого Caddy не получит сертификат.

## 1. Подготовить сервер

```sh
sudo apt update && sudo apt upgrade -y
sudo apt install -y git make curl

# Docker и Compose (официальный скрипт Docker)
curl -fsSL https://get.docker.com | sudo sh
sudo usermod -aG docker "$USER"     # затем выйти и зайти по SSH снова
docker compose version              # нужна 2.24.4 или новее
```

Файрвол:

```sh
sudo ufw allow OpenSSH
sudo ufw allow 80/tcp
sudo ufw allow 443/tcp
sudo ufw allow 443/udp      # HTTP/3
sudo ufw allow 7881/tcp     # LiveKit: ICE по TCP
sudo ufw allow 7882/udp     # LiveKit: медиа
sudo ufw enable
```

Порты, которые публикует Docker, идут мимо ufw. Поэтому в compose
публично опубликованы только перечисленные выше, остальные привязаны к
127.0.0.1.

Если памяти меньше 4 ГБ:

```sh
sudo fallocate -l 4G /swapfile && sudo chmod 600 /swapfile
sudo mkswap /swapfile && sudo swapon /swapfile
echo '/swapfile none swap sw 0 0' | sudo tee -a /etc/fstab
```

## 2. Код

Ядро и клиент лежат рядом в одной папке:

```sh
mkdir -p ~/ruscord && cd ~/ruscord
git clone https://github.com/eastsidekillas/ruscord-core.git
git clone https://github.com/eastsidekillas/ruscord-fe.git
cd ruscord-core
```

## 3. Настройки

```sh
make staging-env          # .env со случайными секретами
nano .env                 # RUSCORD_DOMAIN=ваш.домен
```

- `.env` содержит все секреты. Сохраните копию в надёжном месте.
  - Без `RUSCORD_JWT_ED25519_SEED` все выйдут из аккаунтов.
  - Без ключей S3 пропадёт доступ к загруженным файлам.
- `.env` не коммитится.

## 4. Запуск

```sh
make staging-up           # первая сборка — 5–15 минут
make staging-check        # сайт, API, LiveKit и порты
```

`make staging-check` должен вывести только строки `ok`. Затем откройте
`https://ваш.домен`, зарегистрируйтесь и пригласите друзей ссылкой на
сервер.

**Регистрация открыта всем**, кто знает адрес: закрытой регистрации в ядре
пока нет.

Логи: `make staging-logs`. Остановить: `make staging-down` (данные
останутся в томах Docker).

## 5. Обновление

```sh
cd ~/ruscord/ruscord-fe && git pull
cd ~/ruscord/ruscord-core && git pull
make staging-up && make staging-check
```

Миграции базы применяются при старте ядра.

## 6. Резервные копии

```sh
make staging-backup       # дамп базы и файлы в backups/
```

Раз в сутки по cron (`crontab -e`):

```
30 3 * * * cd ~/ruscord/ruscord-core && make staging-backup >> backups/cron.log 2>&1
```

- Копии лежат на том же сервере. Уносите их на другую машину, например
  `rsync -a server:~/ruscord/ruscord-core/backups/ ./ruscord-backups/`.
- Восстановление: `make staging-restore DIR=backups/<время>`. Перед этим
  остановите ядро: `docker compose --env-file .env -f deploy/docker-compose.yml
  -f deploy/staging/compose.yml --profile app stop app`. Подробности —
  ADR-0022.

## LiveKit Cloud вместо своего LiveKit

Голос, видео и экран могут идти через LiveKit Cloud вместо LiveKit на этом
сервере (ADR-0034). В облаке есть TURN и регионы, поэтому звук пройдёт и из
сетей, где открыт только 443. Медиа при этом идут через серверы LiveKit.

1. Заведите проект на cloud.livekit.io. Нужен **отдельный проект на каждое
   окружение** (тестовый сервер, локальная разработка): иначе сверка ядра
   выводит людей из чужих комнат.
2. В настройках проекта создайте API-ключ и скопируйте:
   - URL проекта (`wss://<проект>.livekit.cloud`);
   - ключ;
   - секрет.
3. В `.env`:

   ```
   RUSCORD_LIVEKIT_MODE=cloud
   RUSCORD_LIVEKIT_CLOUD_URL=wss://<проект>.livekit.cloud
   RUSCORD_LIVEKIT_API_KEY=<ключ проекта>
   RUSCORD_LIVEKIT_API_SECRET=<секрет проекта>
   ```

4. В панели проекта: **Settings → Webhooks → Create new webhook**:
   - URL `https://ваш.домен/livekit/webhook`;
   - Signing API key — тот же ключ.
5. `make staging-up`, затем `make staging-check`. Проверка покажет, что
   вебхук доходит до ядра и проект отвечает.

Порты 7881/tcp и 7882/udp в режиме облака не нужны, их можно закрыть в
файрволе. Вернуться к своему LiveKit — `RUSCORD_LIVEKIT_MODE=local` и
`make staging-up`.

Так же работает и локальный стек (`make up-all`): с `RUSCORD_LIVEKIT_MODE=cloud`
LiveKit в Docker не запускается. Вебхуки до ядра на localhost облако не
доставит, состояния тогда обновляет сверка раз в 30 с.

## Если что-то не работает

| Признак | Что проверить |
|---|---|
| Нет HTTPS, Caddy пишет об ошибке сертификата | A-запись домена указывает на сервер; порт 80 открыт (Let's Encrypt проверяет по нему); `make staging-logs` |
| Сайт открывается, вход не работает | `RUSCORD_DOMAIN` в `.env` совпадает с адресом в браузере (от него зависит `RUSCORD_ALLOWED_ORIGINS`) |
| В голосе «Подключение…» и дальше ничего | `make staging-check`: LiveKit отвечает на `/livekit/`; в логах `livekit` нет ошибок ключей |
| Подключились, но друг друга не слышно | Открыты 7882/udp и 7881/tcp у провайдера VPS (у некоторых свой файрвол в панели); в `.env` задайте `RUSCORD_LIVEKIT_NODE_IP=<IP сервера>` и `RUSCORD_LIVEKIT_EXTERNAL_IP=false`, затем `make staging-up` |
| У кого-то из друзей не работает звук, у остальных работает | Сеть, где закрыт UDP и все порты, кроме 443 (корпоративная, некоторые VPN). TURN на своём сервере пока выключен (ADR-0033); в LiveKit Cloud он есть |
| LiveKit Cloud: в голосе «Подключение…» | В консоли браузера нет ошибок Content-Security-Policy; `RUSCORD_LIVEKIT_CLOUD_URL`, ключ и секрет — одного проекта |
| LiveKit Cloud: в списке канала не видно, кто зашёл, появляется через полминуты | Вебхук в панели проекта: URL `https://ваш.домен/livekit/webhook` и тот же ключ подписи; `make staging-check` |

## Что пока не сделано

- TURN на своём сервере для сетей, где закрыто всё, кроме 443 (ADR-0033); обходной путь — LiveKit Cloud.
- Закрытая регистрация или регистрация по приглашениям.
- Мониторинг. Метрики ядра есть на сервере: `curl 127.0.0.1:9090/metrics`.
