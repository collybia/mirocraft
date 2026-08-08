# API Guide — документация для пользователей

Это гайд, который пойдёт в публичную документацию проекта (раздел «API»).
Стиль: короткие объяснения + рабочие примеры curl. Всё проверяемо копипастой.

---

## Быстрый старт

### 1. Получи токен

В панели: **Настройки → API-токены → Создать**. Выбери scope — для начала
хватит `servers:read` и `servers:power`. Токен показывается один раз, сохрани его.

```bash
export MIROCRAFT=https://mirocraft.example.com/api/v1
export TOKEN=mcr_xxxxxxxxxxxxxxxx
```

### 2. Первый запрос

```bash
curl -s $MIROCRAFT/servers -H "Authorization: Bearer $TOKEN"
```

```json
{
  "items": [
    {
      "id": "01J8ZK3W9X6T2P4Q8R1S5V7Y9A",
      "name": "survival",
      "core": "paper",
      "version": "1.21.4",
      "status": "running",
      "metrics": { "players_online": 5, "tps": 19.97, "ram_used_mb": 2311 }
    }
  ],
  "next_cursor": null
}
```

### 3. Управляй сервером

```bash
# Запустить
curl -s -X POST $MIROCRAFT/servers/$ID/power \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"action": "start"}'

# Остановить с таймаутом 30с (потом kill)
curl -s -X POST $MIROCRAFT/servers/$ID/power \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"action": "stop", "timeout_seconds": 30}'

# Команда в консоль
curl -s -X POST $MIROCRAFT/servers/$ID/command \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"command": "say Перезагрузка через 5 минут!"}'
```

Power-операции асинхронные — в ответ приходит `task_id`:

```bash
curl -s $MIROCRAFT/tasks/$TASK_ID -H "Authorization: Bearer $TOKEN"
# {"id":"...","kind":"power.start","status":"done","progress":100}
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

Без `"eula_accepted": true` вернётся 422 — принятие EULA Mojang обязательно.

### Установить плагин из каталога

```bash
# Найти
curl -s "$MIROCRAFT/catalog/search?q=essentials&type=plugin&mc=1.21.4" \
  -H "Authorization: Bearer $TOKEN"

# Поставить (зависимости подтянутся автоматически)
curl -s -X POST $MIROCRAFT/servers/$ID/catalog/install \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"project_id": "essentialsx"}'
```

### Бэкап перед экспериментами

```bash
curl -s -X POST $MIROCRAFT/servers/$ID/backups \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"note": "перед установкой модов"}'
```

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

### Залить файл

```bash
curl -s -X POST $MIROCRAFT/servers/$ID/files/upload \
  -H "Authorization: Bearer $TOKEN" \
  -F "path=/plugins" \
  -F "file=@MyPlugin.jar"
```

---

## Консоль по WebSocket

Основной токен в URL не передаётся — сначала берётся одноразовый тикет:

```bash
TICKET=$(curl -s -X POST $MIROCRAFT/servers/$ID/console/ticket \
  -H "Authorization: Bearer $TOKEN" | jq -r .ticket)
```

```javascript
const ws = new WebSocket(`wss://mirocraft.example.com/api/v1/servers/${id}/console?token=${ticket}`);

ws.onmessage = (e) => {
  const msg = JSON.parse(e.data);
  if (msg.type === "line")    console.log(msg.text);
  if (msg.type === "metrics") updateDashboard(msg.data);
  if (msg.type === "status")  setStatus(msg.status);
};

ws.send(JSON.stringify({ type: "command", text: "list" }));
```

Пример на Python (websockets):

```python
import asyncio, json, websockets

async def main():
    async with websockets.connect(url) as ws:
        await ws.send(json.dumps({"type": "command", "text": "list"}))
        async for frame in ws:
            msg = json.loads(frame)
            if msg["type"] == "line":
                print(msg["text"])

asyncio.run(main())
```

---

## Webhooks: свои интеграции

Хочешь получать событие «игрок зашёл» в свой сервис:

```bash
curl -s -X POST $MIROCRAFT/webhooks \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{
    "url": "https://my-service.example.com/hook",
    "events": ["player.joined", "player.left", "server.crashed"],
    "secret": "whsec_моя_случайная_строка"
  }'
```

Проверка подписи на своей стороне (обязательно!):

```python
import hmac, hashlib

def verify(body: bytes, signature: str, secret: str) -> bool:
    expected = "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, signature)  # X-Mirocraft-Signature
```

---

## Обработка ошибок

Всегда проверяй `error.code`, а не текст сообщения:

```bash
curl -s -X POST $MIROCRAFT/servers/$ID/power -d '{"action":"start"}' ...
```

```json
{ "error": { "code": "server_already_running", "message": "..." } }
```

| Код | Что делать |
|---|---|
| `unauthorized` | токен просрочен/отозван — получи новый |
| `rate_limited` | подожди `Retry-After` секунд |
| `server_already_running` | это не ошибка для твоей логики — сервер уже в нужном состоянии |
| `insufficient_resources` | на хосте не хватает RAM — покажи пользователю |
| `path_traversal_denied` | путь выходит за пределы сервера — баг в твоём клиенте |

---

## Ограничения

- 120 запросов/мин на токен (WebSocket не считается)
- Файлы: текстовое чтение/запись ≤ 2 МБ, загрузка ≤ 1 ГБ
- История консоли по HTTP — последние 500 строк; нужно больше — читай файлы логов через Files API

## Интерактивная документация

Swagger UI доступен на твоей панели: `https://mirocraft.example.com/api/docs` —
там можно выполнять запросы прямо из браузера со своим токеном.
