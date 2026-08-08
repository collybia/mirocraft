# REST API v1 — полная спецификация

Базовый префикс: `/api/v1`
Аутентификация: `Authorization: Bearer <token>`
Формат: JSON (UTF-8). Время — RFC 3339 (UTC). ID — ULID.

## Общие правила

### Формат ошибок

```json
{
  "error": {
    "code": "server_not_found",
    "message": "Server 01J8... does not exist",
    "details": { "field": "port", "reason": "already_in_use" }
  }
}
```

Коды HTTP: 400 (валидация), 401 (нет/просрочен токен), 403 (нет прав),
404, 409 (конфликт состояния: «сервер уже запущен»), 422 (семантика),
429 (rate limit, заголовок Retry-After), 500.

Машиночитаемые коды ошибок: `validation_failed`, `unauthorized`, `forbidden`,
`server_not_found`, `server_already_running`, `server_not_running`,
`port_in_use`, `insufficient_resources`, `core_version_unsupported`,
`path_traversal_denied`, `file_too_large`, `backup_in_progress`,
`rate_limited`, `internal_error`.

### Пагинация

Курсорная, для всех списков: `?limit=50&cursor=<opaque>`.
Ответ: `{ "items": [...], "next_cursor": "..." | null }`. limit максимум 200.

### Фильтрация и сортировка

`?sort=created_at:desc`, фильтры по полям: `?status=running&core=paper`.

### Idempotency

Мутирующие POST принимают заголовок `Idempotency-Key: <uuid>` —
повтор с тем же ключом в течение 24ч возвращает исходный ответ.

### Rate limits

По токену: 120 запросов/мин, WebSocket не считается. Заголовки
`X-RateLimit-Limit / Remaining / Reset`. На /auth/login — 5/мин по IP.

### Версионирование

v1 стабилен после первого релиза. Новые поля в ответах — не ломающее изменение,
клиенты обязаны игнорировать неизвестные поля. Ломающие изменения → /api/v2.

---

## Auth & Tokens

| Метод | Путь | Описание |
|---|---|---|
| POST | /auth/login | `{email, password, totp?}` → `{token, expires_at, user}` (сессионный токен) |
| POST | /auth/logout | отозвать текущий сессионный токен |
| POST | /auth/refresh | продлить сессионный токен |
| GET | /auth/me | текущий пользователь, роли, лимиты |
| GET | /auth/tokens | список API-токенов (без значений) |
| POST | /auth/tokens | `{name, expires_at?, scopes[]}` → значение токена показывается один раз |
| DELETE | /auth/tokens/{id} | отозвать |
| POST | /auth/totp/enable | включить 2FA: → secret + QR, подтверждение кодом |
| POST | /auth/totp/disable | выключить 2FA |

### Scopes токенов

`servers:read`, `servers:write`, `servers:power`, `servers:console`,
`files:read`, `files:write`, `backups:read`, `backups:write`,
`admin:*`. Сессионный токен из веб-логина имеет все scope роли.

---

## Users & Permissions

| Метод | Путь | Описание |
|---|---|---|
| GET | /users/me | профиль (включая `theme` и `custom_themes`) |
| PATCH | /users/me | смена email/пароля (пароль — с подтверждением старого), смена `theme` |
| GET | /servers/{id}/subusers | соакаунты сервера |
| POST | /servers/{id}/subusers | `{email, permissions[]}` — приглашение по email |
| PATCH | /servers/{id}/subusers/{uid} | изменить права |
| DELETE | /servers/{id}/subusers/{uid} | удалить доступ |

Permissions субпользователя: `power`, `console`, `files.read`, `files.write`,
`backups`, `settings`, `players`.

### Тема оформления

Выбор темы хранится в профиле, чтобы переживать смену браузера. Поле `theme`
в `/users/me` — id встроенной темы (`dark`, `light`, `midnight`, `grass`, `nether`),
`system` (следовать `prefers-color-scheme`; дефолт для новых пользователей)
или `custom:<id>` для собственной темы пользователя.

| Метод | Путь | Описание |
|---|---|---|
| GET | /themes | встроенные темы: id, name, base (`dark`\|`light`), превью-цвета |
| GET | /users/me/themes | кастомные темы пользователя |
| POST | /users/me/themes | создать/импортировать: тело — объект темы (см. ниже) |
| PATCH | /users/me/themes/{tid} | изменить |
| DELETE | /users/me/themes/{tid} | удалить (если была активной — откат на `base`) |

Объект темы (он же формат экспорта/импорта):

```json
{
  "schema": "mirocraft.theme/v1",
  "name": "My Theme",
  "base": "dark",
  "vars": { "--accent": "#7c5cff", "--accent-hover": "#6a4ce0", "--radius": "12px" }
}
```

Валидация при создании и импорте: имена в `vars` — только из whitelist контракта
токенов, значения — только цвета и длины (`url()`, `expression()` и произвольный CSS
отклоняются с 422 `validation_failed`). Пары, не проходящие WCAG AA, не блокируют
сохранение, но возвращаются в `details.contrast_warnings` — панель показывает
предупреждение пользователю.

---

## Servers

### CRUD

| Метод | Путь | Описание |
|---|---|---|
| GET | /servers | список (фильтры: status, core) |
| POST | /servers | создать |
| GET | /servers/{id} | детали |
| PATCH | /servers/{id} | изменить (name, ram_mb, java_args, auto_start, auto_restart) |
| DELETE | /servers/{id}?confirm={name} | удалить вместе с данными |

Тело создания:

```json
{
  "name": "survival",
  "core": "paper",            // vanilla|paper|purpur|fabric|forge|neoforge
  "version": "1.21.4",
  "ram_mb": 4096,
  "port": 25565,              // опционально, иначе автоназначение
  "java_args": "",            // опционально, есть безопасный default (Aikar flags)
  "eula_accepted": true       // обязательное поле, без него 422
}
```

Объект сервера в ответах:

```json
{
  "id": "01J8...",
  "name": "survival",
  "core": "paper",
  "version": "1.21.4",
  "status": "running",        // creating|stopped|starting|running|stopping|crashed
  "ram_mb": 4096,
  "port": 25565,
  "created_at": "...",
  "owner_id": "01J7...",
  "auto_start": true,
  "auto_restart": true,
  "metrics": {
    "ram_used_mb": 2311,
    "cpu_percent": 41.2,
    "uptime_seconds": 86400,
    "players_online": 5,
    "players_max": 20,
    "tps": 19.97
  }
}
```

### Power

| Метод | Путь | Описание |
|---|---|---|
| POST | /servers/{id}/power | `{action: "start"|"stop"|"restart"|"kill", timeout_seconds?}` |

Ответ 202 + `{ "task_id": "..." }` — операция асинхронная, статус через
/tasks/{id} или события.

### Console

| Метод | Путь | Описание |
|---|---|---|
| GET | /servers/{id}/console/history?lines=500 | последние строки лога |
| POST | /servers/{id}/command | `{command: "say hi"}` |
| WS | /servers/{id}/console | двунаправленный стрим, протокол ниже |

Протокол WebSocket (JSON-кадры):

```json
// сервер → клиент
{ "type": "line", "ts": "...", "stream": "stdout", "text": "[12:00:01] ..." }
{ "type": "status", "status": "running" }
{ "type": "metrics", "data": { ...как в metrics выше... } }   // раз в 5с
// клиент → сервер
{ "type": "command", "text": "list" }
```

Аутентификация WS: `?token=` одноразовый тикет, полученный через
`POST /servers/{id}/console/ticket` (TTL 30с) — чтобы не светить основной токен в URL.

### Players

| Метод | Путь | Описание |
|---|---|---|
| GET | /servers/{id}/players | онлайн-игроки (name, uuid, ping) |
| POST | /servers/{id}/players/{name}/kick | `{reason?}` |
| POST | /servers/{id}/players/{name}/ban | `{reason?}` |
| DELETE | /servers/{id}/players/{name}/ban | разбан |
| GET | /servers/{id}/whitelist | список |
| POST | /servers/{id}/whitelist | `{name}` добавить |
| DELETE | /servers/{id}/whitelist/{name} | убрать |
| PATCH | /servers/{id}/whitelist | `{enabled: bool}` вкл/выкл |
| POST | /servers/{id}/ops | `{name}` выдать op |
| DELETE | /servers/{id}/ops/{name} | забрать op |

### Settings (server.properties как API)

| Метод | Путь | Описание |
|---|---|---|
| GET | /servers/{id}/settings | распарсенный server.properties + схема (тип, описание, enum) |
| PATCH | /servers/{id}/settings | частичное обновление, валидация по схеме |

---

## Files

Все пути относительны корня сервера. Канонизация обязательна,
`..`/симлинки наружу → 403 `path_traversal_denied`.

| Метод | Путь | Описание |
|---|---|---|
| GET | /servers/{id}/files?path=/ | листинг: name, type, size, modified_at |
| GET | /servers/{id}/files/content?path= | содержимое (текст ≤ 2 МБ) |
| PUT | /servers/{id}/files/content?path= | записать текст |
| GET | /servers/{id}/files/download?path= | скачать файл (стрим) |
| POST | /servers/{id}/files/upload | multipart, поле path + file, до 1 ГБ |
| POST | /servers/{id}/files/mkdir | `{path}` |
| POST | /servers/{id}/files/move | `{from, to}` |
| POST | /servers/{id}/files/copy | `{from, to}` |
| DELETE | /servers/{id}/files?path= | удалить (рекурсивно для папок) |
| POST | /servers/{id}/files/archive | `{paths[]}` → создаёт zip, возвращает path |
| POST | /servers/{id}/files/unarchive | `{path, destination}` zip/tar.gz |

---

## Backups

| Метод | Путь | Описание |
|---|---|---|
| GET | /servers/{id}/backups | список: id, size, created_at, note, state |
| POST | /servers/{id}/backups | `{note?}` → 202 + task_id |
| GET | /servers/{id}/backups/{bid}/download | стрим архива |
| POST | /servers/{id}/backups/{bid}/restore | 202; сервер должен быть stopped |
| DELETE | /servers/{id}/backups/{bid} | удалить |
| GET | /servers/{id}/backups/schedule | текущее расписание |
| PUT | /servers/{id}/backups/schedule | `{cron: "0 4 * * *", keep_last: 7, enabled: true}` |

---

## Scheduler (задачи по расписанию)

| Метод | Путь | Описание |
|---|---|---|
| GET | /servers/{id}/schedules | список |
| POST | /servers/{id}/schedules | `{name, cron, actions[]}` |
| PATCH | /servers/{id}/schedules/{sid} | изменить/вкл-выкл |
| DELETE | /servers/{id}/schedules/{sid} | удалить |

Action: `{type: "command"|"power"|"backup", payload: {...}}` — выполняются по порядку.
Пример: ежедневный рестарт с предупреждением игрокам.

---

## Cores & Catalog

| Метод | Путь | Описание |
|---|---|---|
| GET | /cores | ядра и поддерживаемые версии MC |
| GET | /cores/{core}/versions | версии + builds (для paper) |
| GET | /catalog/search?q=&type=plugin\|mod\|modpack&mc=&loader= | прокси Modrinth с кэшем |
| GET | /catalog/projects/{pid} | детали проекта, версии, зависимости |
| POST | /servers/{id}/catalog/install | `{project_id, version_id?}` — сам решает plugins/ vs mods/, ставит зависимости |
| GET | /servers/{id}/installed | установленные плагины/моды (скан папки + метаданные) |
| DELETE | /servers/{id}/installed/{file} | удалить |
| POST | /servers/{id}/installed/{file}/toggle | вкл/выкл (переименование .jar ↔ .jar.disabled) |

---

## Tasks (асинхронные операции)

| Метод | Путь | Описание |
|---|---|---|
| GET | /tasks/{id} | `{id, kind, status: queued|running|done|failed, progress: 0-100, error?}` |
| GET | /servers/{id}/tasks | последние задачи сервера |

---

## Events

### WebSocket-шина

`WS /events` (тикет как у консоли) — все события по серверам пользователя:

```json
{ "type": "server.status_changed", "server_id": "...", "data": { "from": "starting", "to": "running" } }
{ "type": "server.crashed", "server_id": "...", "data": { "exit_code": 1 } }
{ "type": "backup.completed", "server_id": "...", "data": { "backup_id": "..." } }
{ "type": "player.joined", "server_id": "...", "data": { "name": "Steve" } }
{ "type": "player.left", "server_id": "...", "data": { "name": "Steve" } }
{ "type": "task.updated", "data": { "task_id": "...", "progress": 40 } }
```

### Webhooks

| Метод | Путь | Описание |
|---|---|---|
| GET | /webhooks | список |
| POST | /webhooks | `{url, events[], secret}` |
| DELETE | /webhooks/{id} | удалить |
| POST | /webhooks/{id}/test | отправить тестовое событие |

Доставка: POST JSON, подпись `X-Mirocraft-Signature: sha256=<hmac>`,
ретраи 3 раза с экспоненциальной задержкой. На этом строится интеграция ботов.

---

## Metrics & Health

| Метод | Путь | Описание |
|---|---|---|
| GET | /health | без авторизации: `{status: "ok", version}` |
| GET | /system | admin: CPU/RAM/диск хоста, число серверов, версия, доступность Docker |
| GET | /servers/{id}/metrics/history?period=1h\|24h\|7d | таймсерия RAM/CPU/players/TPS |
| GET | /metrics | Prometheus-формат (включается в конфиге, отдельный порт) |

---

## Admin

| Метод | Путь | Описание |
|---|---|---|
| GET | /admin/users | все пользователи |
| POST | /admin/users | создать `{email, password, role, limits}` |
| PATCH | /admin/users/{id} | роль, лимиты (max_servers, max_ram_mb, max_disk_mb), блокировка |
| DELETE | /admin/users/{id} | удалить (серверы — переназначить или удалить, ?servers=delete\|transfer:{uid}) |
| GET | /admin/servers | все серверы всех пользователей |
| GET | /admin/audit?user=&action=&from=&to= | журнал: кто, что, когда, с какого IP |
| GET | /admin/settings | глобальные настройки панели |
| PATCH | /admin/settings | регистрация вкл/выкл, дефолтные лимиты, порт-диапазон |

Audit-лог пишется на каждое мутирующее действие автоматически (middleware).

---

## OpenAPI

Спецификация генерируется из кода (swaggo или oapi-codegen — контракт-first
предпочтительнее: openapi.yaml в репозитории, из него генерятся хендлеры и клиенты).
Раздаётся на `/api/v1/openapi.json`, Swagger UI — на `/api/docs`.

## Правила эволюции

- Изменил эндпоинт — обнови этот файл и openapi.yaml в том же коммите
- Новые поля в ответах — можно; удаление/переименование — только в v2
- Каждый эндпоинт покрыт хотя бы одним интеграционным тестом
