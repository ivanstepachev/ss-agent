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
| `-listen` | HTTP API (`/adduser`, `/deleteuser`, `/reload`) | `127.0.0.1:8080` |
| `-db-path` | SQLite база | `/var/lib/inconnect-agent/ports.db` |
| `-min-port`, `-max-port` | Порт прослушки и количество слотов (см. ниже) | `50001–50250` |
| `-public-ip` | IP, отдаваемый в `/adduser` | пусто |
| `-auth-token` | Требуемый заголовок `X-Auth-Token` | пусто (без авторизации) |
| `-docker-image` | Образ Xray | `teddysun/xray:latest` |
| `-container-name` | Имя Docker-контейнера | `xray-ss2022` |
| `-config-dir` | Каталог с `config.json` | `/etc/xray` |

Полный список см. `cmd/inconnect-agent/config.go`.

## Запуск
1. Создать каталоги:
   ```bash
   sudo mkdir -p /var/lib/inconnect-agent /etc/xray
   sudo chown $USER /var/lib/inconnect-agent /etc/xray   # заменить на нужного пользователя
   ```
2. Убедиться, что Docker доступен (тот же пользователь должен иметь права `docker`).
3. Запустить агент:
   ```bash
   sudo ./bin/inconnect-agent \
     -public-ip=203.0.113.10 \
     -auth-token=SECRET_TOKEN
   ```
4. На старте агент:
   - инициализирует БД и 250 слотов;
   - формирует общий inbound на **одном порту** (`min-port`) и добавляет все слоты как `clients`;
   - проверяет конфиг `xray -test`, заменяет `/etc/xray/config.json`;
   - запускает `xray-ss2022` (порты: `min-port` и `api-port`).

### Пример systemd unit (упрощённый)
```
[Unit]
Description=Inconnect SS2022 agent
After=network-online.target docker.service

[Service]
ExecStart=/usr/local/bin/inconnect-agent -public-ip=203.0.113.10 -auth-token=SECRET
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
  - `method`, `ip`.

- `/deleteuser`
  ```bash
  curl -XPOST -H "Content-Type: application/json" \
       -H "X-Auth-Token: SECRET" \
       -d '{"port":50037}' http://127.0.0.1:8080/deleteuser
  ```
  Помечает слот как `reserved`.

- `/reload`
  ```bash
  curl -XPOST -H "X-Auth-Token: SECRET" http://127.0.0.1:8080/reload
  ```
  Возвращает `202 Accepted` сразу и запускает reload асинхронно. Агент:
    - меняет пароли у `reserved` → `free`;
    - заново собирает конфиг (все слоты как `clients`);
    - валидирует его и шлёт `SIGUSR1` в контейнер (быстрый reload без разрыва). При ошибке падает обратно на `docker restart`.

`/healthz` — GET, возвращает `{"status":"ok"}`; нужен для проверок живости.

## Автоматическая установка
Скрипт `scripts/install.sh` сворачивает все шаги в один запуск:
- ставит зависимости (`git`, `golang-go`, `docker.io`, `curl`);
- создаёт пользователя/группу `inconnect`;
- клонирует репозиторий, собирает бинарь и кладёт его в `/usr/local/bin`;
- готовит каталоги `/var/lib/inconnect-agent` и `/etc/xray`;
- подтягивает Docker-образ Xray, пишет unit-файл systemd и запускает службу.

Перед запуском обязательно задайте `REPO_URL` (и при необходимости другие параметры) через переменные окружения:
```bash
sudo REPO_URL=https://github.com/your-org/inconnect-agent.git \
     PUBLIC_IP=203.0.113.10 \
     AUTH_TOKEN=SECRET_TOKEN \
     ./scripts/install.sh
```
Доступные переменные: `BRANCH`, `INSTALL_DIR`, `MIN_PORT`, `MAX_PORT`, `DOCKER_IMAGE`, `LISTEN_ADDR`, `DB_PATH`, `CONFIG_DIR`, и т.д. — см. начало скрипта.

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
3. Пометить и перезагрузить:
   ```bash
   curl -XPOST -H "Content-Type: application/json" \
        -H "X-Auth-Token: SECRET" \
        -d '{"port":<slot_from_adduser>}' http://127.0.0.1:8080/deleteuser
   curl -XPOST -H "X-Auth-Token: SECRET" http://127.0.0.1:8080/reload
   journalctl -u inconnect-agent -n 20
   ```
   В журналах появятся строки `async reload finished` и `reserved processed=N`.

## Примечания
- В БД автоматически создаётся таблица `metadata` с серверным паролем (`server_psk`) для единого inbound-а. При первом запуске значение генерируется и сохраняется.
- `min-port` определяет фактический порт прослушки Shadowsocks. `max-port` задаёт количество слотов (например, `50001–50250` = 250 слотов).
- Все слоты (даже `free`) присутствуют в конфиге как `clients`, поэтому `/adduser` не требует reload.
- Для клиентов SS2022 пароль уже формируется в ответе как `<server_psk>:<client_psk>`; дополнительные вычисления не нужны.
- `/reload` асинхронный: HTTP-ответ приходит сразу, а прогресс виден в `journalctl -u inconnect-agent`.