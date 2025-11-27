# inconnect-agent

## Требования
- Go 1.21+
- Docker CLI (агент вызывает `docker run`, `docker restart`, `docker inspect`)
- Образ `xray:latest` (или другой, указанный флагом `-docker-image`)
- Доступная запись в каталоги:
  - `/var/lib/inconnect-agent` — база `ports.db`
  - `/etc/xray` — `config.json` и временные файлы

## Сборка
```bash
go build -o bin/inconnect-agent ./cmd/inconnect-agent
```

## Конфигурация
Все параметры задаются флагами, главные:

| Флаг | Описание | По умолчанию |
| --- | --- | --- |
| `-config` | Путь к YAML/JSON файлу конфигурации. Если не задан, используется `/etc/inconnect-agent/config.yaml` (если существует) или `INCONNECT_CONFIG`. | пусто |
| `-listen` | HTTP API (`/adduser`, `/deleteuser`, `/reload`) | `127.0.0.1:8080` |
| `-db-path` | SQLite база | `/var/lib/inconnect-agent/ports.db` |
| `-min-port`, `-max-port` | Порт базы и общее число слотов (если не задан `-shards`) | `50001–50250` |
| `-shard-count` / `-shard-size` | Кол-во шардов и слотов в каждом (по умолчанию всё в одном) | `1` / `portCount` |
| `-shard-port-step` | Разница между портами шардов | `1` |
| `-shards` | Явное описание `port:slots,...` (перекрывает предыдущие) | пусто |
| `-shard-prefix` | Префикс для имён контейнеров | `xray-ss2022` |
| `-restart-interval` | Авто-рестарт (с пересборкой) раз в N секунд (0 = выкл) | `0` |
| `-restart-when-reserved` | Перезапуск конкретного шарда, когда в нём ≥ N `reserved`-слотов (0 = выкл) | `0` |
| `-restart-at` | Список времён по UTC (`HH:MM,HH:MM`), когда запускать рестарт всех шардов | пусто |
| `-allocation-strategy` | Распределение слотов: `sequential` / `roundrobin` / `leastfree` | `roundrobin` |
| `-reset` | Выполнить полный сброс БД/шардов и завершить работу | `false` |
| `-public-ip` | IP, отдаваемый в `/adduser` | пусто |
| `-auth-token` | Требуемый заголовок `X-Auth-Token` | пусто (без авторизации) |
| `-docker-image` | Образ Xray | `teddysun/xray:latest` |
| `-config-dir` | Каталог с конфигами | `/etc/xray` |

Полный список см. `cmd/inconnect-agent/config.go`. Все параметры можно задать через файл `config.yaml` (YAML/JSON). Алгоритм чтения:

1. Агент берёт значения по умолчанию.
2. Если найден конфиг (`-config`, `INCONNECT_CONFIG` или `/etc/inconnect-agent/config.yaml`), он загружается и дополняет/переопределяет дефолты.
3. Флаги командной строки имеют наивысший приоритет (перебивают файл).

Пример `config.yaml`:
```yaml
listen: 127.0.0.1:8080
dbPath: /var/lib/inconnect-agent/ports.db
configDir: /etc/xray
publicIP: 203.0.113.10
authToken: SECRET
dockerImage: teddysun/xray:latest
shardCount: 8
shardSize: 500
shardPortStep: 10
shardPrefix: xray-ss2022
restartInterval: 0
restartWhenReserved: 50
restartAt:
  - "02:00"
  - "14:00"
allocationStrategy: roundrobin
```
Запуск:
```bash
sudo ./bin/inconnect-agent -config=/etc/inconnect-agent/config.yaml
```
Если установлен `INCONNECT_CONFIG=/etc/inconnect-agent/config.yaml`, флаг можно не передавать.

### Шардинг
- По умолчанию агент работает в одном контейнере: все слоты добавляются в `clients` на `min-port`.
- Для продакшена можно разбить базу на шарды (например, `SHARD_COUNT=8`, `SHARD_SIZE=500`), чтобы каждый контейнер обслуживал 500 клиентов на своём порту (`50010`, `50020`, ...).
- Порты вычисляются как `min-port + (shard-1)*shard-port-step`, но при необходимости можно задать явный список `-shards=50010:500,50050:1000,...`.
- Каждому шару выдаётся собственный `server_psk` и Docker-контейнер `shard-prefix-<id>`, поэтому reload и падения одного контейнера не влияют на остальные.

## Запуск
1. Создать каталоги:
   ```bash
   sudo mkdir -p /var/lib/inconnect-agent /etc/xray
   sudo chown $USER /var/lib/inconnect-agent /etc/xray   # заменить на нужного пользователя
   ```
2. Убедиться, что Docker доступен (тот же пользователь должен иметь права `docker`).
3. Создать конфиг (если ещё не создан) и запустить агент:
   ```bash
   sudo tee /etc/inconnect-agent/config.yaml >/dev/null <<'EOF'
listen: 127.0.0.1:8080
dbPath: /var/lib/inconnect-agent/ports.db
publicIP: 203.0.113.10
authToken: SECRET_TOKEN
EOF
   sudo ./bin/inconnect-agent -config=/etc/inconnect-agent/config.yaml
   ```
4. На старте агент:
   - инициализирует БД и создаёт слоты по каждому шару (по умолчанию 1×`max-port - min-port + 1`);
   - для каждого шарда формирует отдельный конфиг (`/etc/xray/config-shard-<n>.json`) с inbound на своём порту и собственным server PSK;
   - проверяет конфиги `xray -test`, активирует их и создаёт/перезапускает контейнеры `shard-prefix-<n>` с маппингом только нужных портов.

### Пример systemd unit (упрощённый)
```
[Unit]
Description=Inconnect SS2022 agent
After=network-online.target docker.service

[Service]
ExecStart=/usr/local/bin/inconnect-agent -config=/etc/inconnect-agent/config.yaml
Restart=always
User=inconnect
Group=inconnect

[Install]
WantedBy=multi-user.target
```

## HTTP API
Все запросы `POST`, JSON.

- `/adduser`
  ```bash
  curl -XPOST -H "Content-Type: application/json" \
       -H "X-Auth-Token: SECRET" \
       -d '{"user_id":"123"}' http://127.0.0.1:8080/adduser
  ```
  Ответ содержит:
  - `listenPort` — фактический порт Shadowsocks (общий для всех клиентов);
  - `slotId` — идентификатор слота (его же нужно передавать в `/deleteuser`);
  - `password` — значение формата `<server_psk>:<client_psk>` (можно вставлять прямо в клиент);
  - `freeSlots` — сколько слотов осталось свободными суммарно.
- `/deleteuser`
  ```bash
  curl -XPOST -H "Content-Type: application/json" \
       -H "X-Auth-Token: SECRET" \
       -d '{"slotId":50037}' http://127.0.0.1:8080/deleteuser
  ```
  Помечает слот как `reserved`. Можно передать несколько ID сразу:
  ```json
  { "slotIds": [50037, 50038, 50040] }
  ```

- `/reload`
  ```bash
  curl -XPOST -H "X-Auth-Token: SECRET" http://127.0.0.1:8080/reload
  ```
  Возвращает `202 Accepted` и запускает reload асинхронно. Можно указать конкретный шард:
  ```bash
  curl -XPOST -H "X-Auth-Token: SECRET" \
       -d '{"shardId":2}' \
       http://127.0.0.1:8080/reload
  ```
  Процесс:
    - пароли у `reserved` → `free`;
    - пересборка конфига выбранного шарда;
    - `xray -test` + обновление файла + `SIGUSR1` контейнеру (fallback на `docker restart` при ошибке).
- `/restart`
  ```bash
  curl -XPOST -H "X-Auth-Token: SECRET" http://127.0.0.1:8080/restart
  ```
  Пересобирает конфиг так же, как `/reload`, и сразу выполняет **полный рестарт** шардов (или конкретного, если передать `{"shardId":2}`), чтобы мгновенно сбросить все текущие TCP/UDP соединения.
- `/reset`
  ```bash
  curl -XPOST -H "X-Auth-Token: SECRET" http://127.0.0.1:8080/reset
  ```
  Асинхронно выполняет полный сброс:
  1. останавливает и удаляет все контейнеры `xray-ss2022-*`;
  2. очищает таблицы `slots` и `metadata`, создаёт новый набор слотов и серверных PSK;
  3. пересобирает конфиги всех шардов и выполняет каскадный рестарт.
  Используйте, когда нужно «начать с нуля» и раздать всем клиентам новые пароли.

- `/stats` (GET)
  ```bash
  curl -H "X-Auth-Token: SECRET" http://127.0.0.1:8080/stats
  ```
  Возвращает состояние каждого шарда и суммарные показатели:
  ```json
  {
    "shards": [
      {"id":1,"port":50010,"free":498,"used":2,"reserved":0},
      ...
    ],
    "totals":{"free":3980,"used":20,"reserved":0}
  }
  ```
  Доступно только при предъявлении `X-Auth-Token`.

`/healthz` — GET, возвращает `{"status":"ok"}`; нужен для проверок живости.

## Автоматическая установка
Скрипт `scripts/install.sh` сворачивает все шаги в один запуск:
- ставит зависимости (`git`, `golang-go`, `docker.io`, `curl`);
- создаёт пользователя/группу `inconnect`;
- клонирует репозиторий, собирает бинарь и кладёт его в `/usr/local/bin`;
- готовит каталоги `/var/lib/inconnect-agent` и `/etc/xray`;
- генерирует `/etc/inconnect-agent/config.yaml` из переданных переменных, записывает unit-файл systemd с `-config` и запускает службу.

Перед запуском обязательно задайте `REPO_URL` (и при необходимости другие параметры) через переменные окружения:
```bash
sudo REPO_URL=https://github.com/your-org/inconnect-agent.git \
     PUBLIC_IP=203.0.113.10 \
     AUTH_TOKEN=SECRET_TOKEN \
     SHARD_COUNT=8 \
     SHARD_SIZE=500 \
     MIN_PORT=50010 \
     SHARD_PORT_STEP=10 \
     RESTART_INTERVAL=600 \
     RESTART_WHEN_RESERVED=50 \
     RESTART_AT="02:00,14:00" \
     ALLOCATION_STRATEGY=roundrobin \
     ./scripts/install.sh
```
Дополнительные переменные:
- `SHARD_COUNT`, `SHARD_SIZE` — количество контейнеров и слотов в каждом (по умолчанию всё в одном).
- `SHARD_PORT_STEP` — шаг между портами шардов (пример выше: 50010, 50020, ...).
- `SHARDS` — явный список `port:slots,...`, если нужно задать разные размеры.
- `SHARD_PREFIX` — как именовать контейнеры (по умолчанию `xray-ss2022` → `xray-ss2022-1`, `-2`, ...).
- `RESTART_INTERVAL` — как часто автоматически запускать `/restart` (в секундах, 0 = отключено).
- `RESTART_WHEN_RESERVED` — рестартует только те шарды, где накопилось ≥ N слотов со статусом `reserved`.
- `RESTART_AT` — список UTC-времён (`HH:MM,HH:MM`), когда выполнять каскадный рестарт всех шардов (1–2 окна в сутки).
- `ALLOCATION_STRATEGY` — стратегия выдачи слотов: `sequential`, `roundrobin`, `leastfree`.
- `CONFIG_FILE` — путь, куда положить `config.yaml` (по умолчанию `/etc/inconnect-agent/config.yaml`).
Полный список см. в начале скрипта (можно задавать и `BRANCH`, `INSTALL_DIR`, `DB_PATH`, `CONFIG_DIR`, и т.д.).

### Быстрое тестирование без удалённого репозитория
Если код находится уже на сервере, можно пропустить `git clone`, указав `LOCAL_SOURCE_DIR` (скрипт просто скопирует текущую директорию):
```bash
sudo LOCAL_SOURCE_DIR=$PWD \
     PUBLIC_IP=203.0.113.10 \
     AUTH_TOKEN=SECRET \
     ./scripts/install.sh
```
Это удобно для проверки свежих сборок до того, как они попадут в GitHub.

## Проверка после установки/обновления
1. Убедиться, что службы запущены:
   ```bash
   systemctl status inconnect-agent
   docker ps | grep xray-ss2022
   ```
2. Получить тестовый слот:
   ```bash
   curl -XPOST -H "Content-Type: application/json" \
        -H "X-Auth-Token: SECRET" \
        -d '{"user_id":"test"}' \
        http://127.0.0.1:8080/adduser
   ```
   Запомните `slotId`, `shardId` и `listenPort` из ответа — пароль уже в формате `<server_psk>:<client_psk>`.
3. Пометить и перезагрузить:
   ```bash
   curl -XPOST -H "Content-Type: application/json" \
        -H "X-Auth-Token: SECRET" \
        -d '{"slotId":<slot_from_adduser>}' http://127.0.0.1:8080/deleteuser
   curl -XPOST -H "X-Auth-Token: SECRET" http://127.0.0.1:8080/reload
   curl -XPOST -H "X-Auth-Token: SECRET" \
        -d '{"shardId":1}' \
        http://127.0.0.1:8080/restart
   journalctl -u inconnect-agent -n 20
   ```
В журналах появятся строки `async reload finished` и `reserved processed=N`.

## Примечания
- В БД автоматически создаётся таблица `metadata` с серверным паролем (`server_psk`) для единого inbound-а. При первом запуске значение генерируется и сохраняется.
- `min-port` определяет фактический порт прослушки Shadowsocks. `max-port` задаёт количество слотов (например, `50001–50250` = 250 слотов).
- Все слоты (даже `free`) присутствуют в конфиге как `clients`, поэтому `/adduser` не требует reload. Ответ содержит `slotId`, `shardId`, `listenPort` и готовый пароль `<server_psk>:<client_psk>`.
- `/reload` асинхронный: HTTP-ответ приходит сразу, а прогресс виден в `journalctl -u inconnect-agent`.
- `/restart` пересобирает конфиг и делает `docker restart` шардов; можно запускать вручную или настроить авто-каскад через `-restart-interval`.
- `/reset` и флаг `-reset` выполняют одинаковый «жёсткий» сброс. Через CLI можно запустить один раз: `sudo inconnect-agent ... -reset`. Через API операция выполняется асинхронно, но блокирует выдачу/удаление до завершения.
- Для фиксированных «ночных» окон можно задать `-restart-at=02:00,14:00` (UTC) — агент сам будет каскадно перезапускаться в эти моменты независимо от аптайма.