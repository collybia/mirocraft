# API — гайд

REST API — не приложение к панели: панель сама его клиент, и боты тоже. Всё, что
можно сделать мышкой, можно сделать curl'ом.

Каждый пример ниже выполнен на живой панели, а не написан по памяти. Ответы —
настоящие, только идентификаторы укорочены.

---

## Быстрый старт

### 1. Получите токен

В панели: **Настройки → API-токены → Создать**. Выберите scope — токен
показывается один раз.

Или через API, сессионным токеном после логина:

```bash
export MIROCRAFT=https://panel.example.com/api/v1

SESSION=$(curl -s -X POST $MIROCRAFT/auth/login \
  -H "Content-Type: application/json" \
  -d '{"email": "admin", "password": "..."}' | jq -r .token)

curl -s -X POST $MIROCRAFT/auth/tokens \
  -H "Authorization: Bearer $SESSION" -H "Content-Type: application/json" \
  -d '{"name": "my script", "scopes": ["servers:read", "servers:power"]}'
```

```json
{
  "id": "01KZKH…",
  "name": "my script",
  "scopes": ["servers:read", "servers:power"],
  "token": "mcr_…"
}
```

```bash
export TOKEN=mcr_…
```

**Берите ровно те scope, которые нужны.** Скрипту, который перезапускает сервер
по ночам, хватает `servers:read` и `servers:power`; создание сервера требует
`servers:write`, файлы — `files:write`, бэкапы — `backups:write`.

### 2. Первый запрос

```bash
curl -s $MIROCRAFT/servers -H "Authorization: Bearer $TOKEN"
```

```json
{
  "items": [
    {
      "id": "01KZKHSEPDN9C6FG84NPXN3R63",
      "name": "modded",
      "core": "fabric",
      "version": "1.21.4",
      "kind": "server",
      "status": "running",
      "ram_mb": 6144,
      "port": 25565,
      "eula_accepted": true,
      "crossplay": false,
      "created_at": "2026-08-09T15:20:47Z"
    }
  ],
  "next_cursor": null
}
```

**Метрик в списке нет** — они стоят похода к процессу или к контейнеру, и платить
за это на каждой карточке не за что. Метрики отдаёт запрос одного сервера:

```bash
curl -s $MIROCRAFT/servers/$ID -H "Authorization: Bearer $TOKEN"
```

```json
{
  "id": "01KZKHSEPDN9C6FG84NPXN3R63",
  "status": "running",
  "metrics": {
    "ram_used_mb": 2319,
    "cpu_percent": 0.35,
    "uptime_seconds": 263,
    "players_online": 0,
    "players_max": 20
  }
}
```

`players_online` и `players_max` — `null`, пока сервер не начал принимать
подключения: они берутся Server List Ping'ом по игровому порту. `cpu_percent` —
доля одного ядра, как в `top`: на прогрузке мира сервер спокойно показывает
несколько сотен процентов, и это не ошибка. **TPS панель не отдаёт** — по
протоколу его узнать нельзя, а угадывать хуже, чем не показывать.

### 3. Управляйте сервером

```bash
# Запустить
curl -s -X POST $MIROCRAFT/servers/$ID/power \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"action": "start"}'

# Остановить с таймаутом 30 с (потом kill)
curl -s -X POST $MIROCRAFT/servers/$ID/power \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"action": "stop", "timeout_seconds": 30}'
```

```json
{ "task_id": "01KZKHSS3FFMPCRMAE0GMWH14T" }
```

Power-операции асинхронные: холодный старт качает ядро и рантайм, и держать ради
этого HTTP-соединение открытым нечестно. Ответ — `202` и `task_id`:

```bash
curl -s $MIROCRAFT/tasks/$TASK_ID -H "Authorization: Bearer $TOKEN"
```

```json
{
  "id": "01KZKHSS3FFMPCRMAE0GMWH14T",
  "kind": "power.start",
  "server_id": "01KZKHSEPDN9C6FG84NPXN3R63",
  "status": "done",
  "progress": 100,
  "created_at": "2026-08-09T15:20:58Z",
  "updated_at": "2026-08-09T15:20:59Z"
}
```

`status` — `queued`, `running`, `done` или `failed`; у упавшей задачи есть
`error`. Задача живёт полчаса после завершения — этого хватает, чтобы её
опросить, и не хватает, чтобы накопить их гигабайт.

### 4. Команда в консоль

```bash
curl -s -X POST $MIROCRAFT/servers/$ID/command \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"command": "say Перезагрузка через 5 минут!"}'
```

Ответ — `204 No Content`: команда ушла в stdin сервера, а что он на неё скажет,
видно в консоли. Тело должно быть **валидным UTF-8** — иначе `400`
`validation_failed`. Это не придирка: cp1251 из Windows-консоли доедет до
сервера и превратится в кракозябры в чате, а отказ виден сразу.

```bash
curl -s "$MIROCRAFT/servers/$ID/console/history?lines=3" \
  -H "Authorization: Bearer $TOKEN"
```

```json
{
  "items": [
    {
      "type": "line",
      "ts": "2026-08-09T15:22:41Z",
      "stream": "stdout",
      "text": "[15:22:41] [Server thread/INFO]: [Not Secure] [Server] Перезагрузка через 5 минут!"
    }
  ]
}
```

---

## Рецепты

### Создать сервер

```bash
curl -s -X POST $MIROCRAFT/servers \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{
    "name": "modded",
    "core": "fabric",
    "version": "1.21.4",
    "ram_mb": 6144,
    "eula_accepted": true
  }'
```

Без `"eula_accepted": true` — `422`: принятие EULA Mojang обязательно, и
принимает его человек, а не панель за него.

Порт можно не указывать — панель выдаст свободный из своего диапазона. Какие
ядра и версии бывают:

```bash
curl -s $MIROCRAFT/cores -H "Authorization: Bearer $TOKEN"
curl -s $MIROCRAFT/cores/fabric/versions -H "Authorization: Bearer $TOKEN"
```

### Поставить плагин или мод

Сначала спросите, что это ядро вообще принимает: Fabric читает `mods/`, Paper —
`plugins/`, а vanilla не читает ничего.

```bash
curl -s $MIROCRAFT/servers/$ID/catalog -H "Authorization: Bearer $TOKEN"
```

```json
{ "loader": "fabric", "dir": "mods", "version": "1.21.4" }
```

Пустой `loader` — это ответ, а не ошибка: рядом с vanilla jar никогда не будет
прочитан.

```bash
# Найти. Плагины на Modrinth published как mod с категорией bukkit/paper,
# поэтому фильтровать надо по loader, а не по несуществующему type=plugin.
curl -s "$MIROCRAFT/catalog/search?q=lithium&type=mod&loader=fabric&mc=1.21.4" \
  -H "Authorization: Bearer $TOKEN"

# Посмотреть, что принесёт установка, ничего не скачивая
curl -s -X POST $MIROCRAFT/servers/$ID/catalog/install \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"project_id": "gvQqBUqZ", "dry_run": true}'

# Поставить (обязательные зависимости подтянутся, необязательные — нет)
curl -s -X POST $MIROCRAFT/servers/$ID/catalog/install \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"project_id": "gvQqBUqZ"}'
```

### Поставить модпак

Один вызов ставит пак, нужный загрузчик и нужную версию Minecraft. Сервер должен
быть остановлен: установка стирает каталог модов.

```bash
curl -s -X POST $MIROCRAFT/servers/$ID/modpack \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"project_id": "vanilla-perfected", "dry_run": true}'
```

```json
{
  "project_id": "1ocGzRHv",
  "version_id": "Bu8RKHri",
  "name": "Chaos Cubed Hotfix 3.0",
  "version": "1.0.3+26.2",
  "file": "Vanilla Perfected 1.0.0+26.3.mrpack",
  "size_bytes": 6337780,
  "core": "fabric",
  "minecraft": "26.2",
  "changes_core": true,
  "replaces_dir": "mods"
}
```

`changes_core` — то, что стоит показать человеку до подтверждения: пак меняет
не только моды, а само ядро и версию. Без `dry_run` ответ — `202` и `task_id`, у
задачи есть `progress` от 0 до 99. На запущенном сервере — `409`
`server_already_running`.

### Бэкап перед экспериментами

```bash
curl -s -X POST $MIROCRAFT/servers/$ID/backups \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"note": "перед установкой модов"}'
```

Ответ — `202` и `task_id`. Второй бэкап того же сервера, пока идёт первый,
отвергается:

```json
{ "error": { "code": "backup_in_progress", "message": "a backup of this server is already running" } }
```

Это и есть защита от дубля: два архиватора, идущие по одному каталогу, дают два
архива, каждый из которых поймал его на середине записи.

### Ежедневный рестарт в 4:00 с предупреждением

```bash
curl -s -X POST $MIROCRAFT/servers/$ID/schedules \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{
    "name": "night restart",
    "cron": "55 3 * * *",
    "actions": [
      {"type": "command", "payload": {"command": "say Рестарт через 5 минут"}},
      {"type": "command", "payload": {"command": "say Рестарт через 1 минуту", "delay_seconds": 240}},
      {"type": "power", "payload": {"action": "restart", "delay_seconds": 60}}
    ]
  }'
```

```json
{
  "id": "01KZKHW7METT5MBR74QXGZS980",
  "name": "night restart",
  "cron": "55 3 * * *",
  "enabled": true,
  "next_run_at": "2026-08-10T03:55:00+03:00"
}
```

`delay_seconds` — пауза **перед** действием, поэтому цепочка выше растянута на
пять минут от одного тика cron. `next_run_at` считается в часовом поясе панели.

### Залить файл

```bash
curl -s -X POST $MIROCRAFT/servers/$ID/files/upload \
  -H "Authorization: Bearer $TOKEN" \
  -F "path=/mods" \
  -F "file=@MyMod.jar"
```

Имя файла берётся из формы и проходит ту же песочницу, что и всё остальное:
`../../evil.jar` не выйдет за каталог сервера, а вернёт `path_traversal_denied`.

---

## Консоль по WebSocket

Основной токен в URL не передаётся — он попал бы в логи прокси и в историю
браузера. Сначала берётся одноразовый тикет, живущий 30 секунд:

```bash
TICKET=$(curl -s -X POST $MIROCRAFT/servers/$ID/console/ticket \
  -H "Authorization: Bearer $TOKEN" | jq -r .ticket)
```

```javascript
const ws = new WebSocket(
  `wss://panel.example.com/api/v1/servers/${id}/console?token=${ticket}`,
);

ws.onmessage = (e) => {
  const msg = JSON.parse(e.data);
  if (msg.type === "line")   console.log(msg.text);   // {ts, stream, text}
  if (msg.type === "status") setStatus(msg.status);   // running | stopped | crashed
  if (msg.type === "error")  console.warn(msg.code, msg.message);
};

// Единственный кадр, который принимает сервер.
ws.send(JSON.stringify({ type: "command", text: "list" }));
```

Кадр `error` — это ответ на отвергнутую команду: без него клиент, чья команда не
прошла, видел бы тишину и не мог отличить её от выполненной.

**Метрик на этом сокете нет.** Они приходят запросом `GET /servers/{id}` —
панель опрашивает его раз в пять секунд, и это же самое стоит делать клиенту.

Пример на Python:

```python
import asyncio, json, websockets

async def main(url):
    async with websockets.connect(url) as ws:
        await ws.send(json.dumps({"type": "command", "text": "list"}))
        async for frame in ws:
            msg = json.loads(frame)
            if msg["type"] == "line":
                print(msg["text"])

asyncio.run(main(url))
```

Вторая шина — `GET /events?token={ticket}` (тикет от `POST /events/ticket`):
события всех ваших серверов одним соединением, вместо консоли на каждый.

---

## Webhooks: свои интеграции

```bash
curl -s -X POST $MIROCRAFT/webhooks \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{
    "url": "https://my-service.example.com/hook",
    "events": ["player.joined", "player.left", "server.crashed"],
    "secret": "whsec_моя_случайная_строка"
  }'
```

```json
{
  "id": "01KZKHW7P1S2JV789F5JTBPZG5",
  "url": "https://my-service.example.com/hook",
  "events": ["player.joined", "player.left", "server.crashed"],
  "enabled": true,
  "has_secret": true,
  "failure_count": 0
}
```

Секрет обратно не отдаётся — только `has_secret`.

События: `server.status_changed`, `server.crashed`, `backup.completed`,
`backup.failed`, `player.joined`, `player.left`, `task.updated`. Неизвестное имя
отвергается, а не принимается молча: подписка на опечатку доставляла бы ноль
событий и выглядела бы как «панель не работает».

Проверка подписи на своей стороне — обязательно:

```python
import hmac, hashlib

def verify(body: bytes, signature: str, secret: str) -> bool:
    expected = "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, signature)  # заголовок X-Mirocraft-Signature
```

Заголовки доставки: `X-Mirocraft-Signature`, `X-Mirocraft-Event`,
`X-Mirocraft-Delivery` (уникальный на доставку — по нему отбрасывается дубль,
который прислала повторная попытка). Попыток три, с удвоением паузы.

Адрес вебхука — пользовательский, поэтому по умолчанию панель отказывается
доставлять на приватные и link-local адреса; включается
`webhooks.allow_private_hosts` в конфиге.

---

## Обработка ошибок

Проверяйте `error.code`, а не текст сообщения: текст может стать понятнее,
код — нет.

```json
{
  "error": {
    "code": "path_traversal_denied",
    "message": "the path is outside the server directory or is not allowed",
    "details": { "field": "path" }
  }
}
```

| Код | HTTP | Что делать |
|---|---|---|
| `validation_failed` | 400/422 | смотрите `details.field` — тело или параметр не тот |
| `unauthorized` | 401 | токен просрочен или отозван — получите новый |
| `forbidden` | 403 | у токена нет нужного scope |
| `server_not_found` | 404 | нет такого сервера **или он чужой** — панель не различает это специально |
| `server_already_running` | 409 | не ошибка для вашей логики: сервер уже в нужном состоянии |
| `server_not_running` | 409 | команда или кик требуют запущенного сервера |
| `backup_in_progress` | 409 | бэкап этого сервера уже идёт, дождитесь задачи |
| `insufficient_resources` | 409/422 | превышен лимит аккаунта: сервера, память или диск. В `details` — `limit`, `used` |
| `path_traversal_denied` | 400 | путь выходит за каталог сервера — баг в вашем клиенте |
| `rate_limited` | 429 | подождите `Retry-After` секунд |

---

## Ограничения

- 120 запросов в минуту на токен; вход — 5 попыток в минуту с адреса.
  WebSocket не считается: одно долгое соединение — не поток запросов.
- Файлы: чтение и запись текста ≤ 2 МБ, загрузка ≤ 1 ГБ, распаковка ≤ 8 ГиБ
  суммарно и ≤ 100 000 записей.
- Модпак: ≤ 2000 файлов, ≤ 512 МБ на файл, ≤ 8 ГБ на пак; загрузка только с
  хостов реестров и только по https.
- История консоли по HTTP — последние 500 строк. Нужно больше — читайте
  `logs/latest.log` через Files API.
- Задачи хранятся 30 минут после завершения.

---

## Интерактивная документация

Swagger UI — на вашей же панели: `https://panel.example.com/api/docs`. Оттуда
можно выполнять запросы своим токеном. Сама спецификация —
`GET /api/v1/openapi.yaml`; она лежит в репозитории и сверяется с роутером
тестом в обе стороны, так что и незадокументированный эндпоинт, и описанный, но
не существующий — падение сборки.

Полный список эндпоинтов с объяснениями, почему они такие, — в
[docs/API.md](API.md).
