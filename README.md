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
| `-min-port`, `-max-port` | Диапазон SS2022 портов | `50001–50250` |
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
   - инициализирует БД и 250 слотов,
   - собирает `config.generated.json`,
   - проверяет его `docker run ... xray -test`,
   - заменяет `/etc/xray/config.json`,
   - создаёт/перезапускает контейнер `xray-ss2022`.

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
  Ответ содержит `port`, `password`, `method`, `ip`.

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
  Перегенерирует пароли для всех `reserved`, пересобирает конфиг, валидирует его и рестартует контейнер.

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

## Примечания
- `ports.db` использует WAL и блокировки SQLite (`_busy_timeout=5000`), поэтому агент рассчитан на единственный экземпляр.
- Порты, находящиеся в статусе `used`, всегда присутствуют в конфиге с тем же паролем; `/adduser` не требует перезагрузки.
- Если в `/reload` не было `reserved`, контейнер всё равно перезапускается (поведение можно доработать при необходимости).