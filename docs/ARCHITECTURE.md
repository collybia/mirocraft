# Архитектура Mirocraft

## Общая схема

```
┌─────────────┐  ┌──────────────┐  ┌───────────────┐
│  Веб-панель │  │ Discord-бот  │  │ Telegram-бот  │
└──────┬──────┘  └──────┬───────┘  └───────┬───────┘
       │                │                  │
       └────────────────┼──────────────────┘
                        ▼
              ┌───────────────────┐
              │     REST API      │   один Go-бинарник
              │  ─────────────    │
              │      Daemon       │
              │  ┌─────────────┐  │
              │  │   Runner    │  │  интерфейс
              │  └──┬───────┬──┘  │
              └─────┼───────┼─────┘
                    ▼       ▼
             DockerRunner  ProcessRunner
             (Linux)       (Windows / без Docker)
```

## Компоненты

### Daemon (internal/daemon)

Владеет состоянием всех серверов. Отвечает за:
- CRUD серверов (создание, удаление, конфигурация)
- Жизненный цикл: start / stop / restart / kill
- Стриминг консоли (stdout/stderr) и отправку команд в stdin
- Скачивание серверного ПО через реестр CoreProvider (интерфейс: список версий + скачивание + метаданные):
  - Vanilla/Snapshot — Mojang version manifest
  - Paper, Folia, Velocity, Waterfall — PaperMC API
  - Purpur — PurpurMC API; Pufferfish — сборки с их CI
  - Spigot — BuildTools (сборка на месте, кэш результата; медленно — предупреждать в UI)
  - Fabric, Quilt — их meta-API + installer
  - Forge, NeoForge — installer с промоушенами версий; запуск через run.sh/run.bat в новых версиях
  - Sponge, Mohist, Arclight — их API/CI
  - BungeeCord — Jenkins CI
  - Bedrock Dedicated Server — официальный сайт Mojang (нативный бинарник linux/windows!)
  - PocketMine-MP — GitHub Releases (+ PHP-рантайм), PowerNukkitX — GitHub Releases
  - Модпаки — Modrinth (.mrpack) и CurseForge (manifest.json): распаковка, докачка модов,
    установка нужного загрузчика автоматически
  Каждый провайдер знает требования рантайма (версия Java / PHP / нативный) и тип
  (server | proxy | bedrock) — от типа зависят дефолтный порт, протокол (TCP/UDP) и логика консоли
- Файловый менеджер в пределах директории сервера (защита от path traversal — обязательна)
- Лимиты ресурсов (RAM всегда; CPU — в DockerRunner через cgroups, в ProcessRunner best-effort)
- Бэкапы (архив директории сервера по расписанию или по запросу)

### Runner (internal/runner)

```go
type Runner interface {
    Start(ctx context.Context, srv *Server) error
    Stop(ctx context.Context, id string, timeout time.Duration) error
    Kill(ctx context.Context, id string) error
    Status(ctx context.Context, id string) (Status, error)
    Console(ctx context.Context, id string) (io.ReadCloser, error)
    SendCommand(ctx context.Context, id string, cmd string) error
}
```

- **DockerRunner**: контейнер на сервер, образ itzg/minecraft-server или свой минимальный
  образ с Java. Лимиты через Docker API. Тома — директория сервера.
- **ProcessRunner**: запуск `java -Xmx... -jar server.jar` как дочернего процесса.
  Менеджер Java-рантаймов: скачивает нужную версию (Temurin) под версию Minecraft,
  т.к. MC 1.8–1.16 требует Java 8/11, 1.17+ — Java 17, 1.20.5+ — Java 21.
  На Windows — job objects для гарантированного убийства дерева процессов.

Выбор Runner — автоопределение при старте демона (есть Docker → DockerRunner),
переопределяется в конфиге.

### REST API (internal/api)

- chi или стандартный net/http (Go 1.22 routing)
- Аутентификация: API-токены (Bearer), в БД — только SHA-256 хэши
- Роли: admin / user; сервер принадлежит пользователю
- WebSocket: /api/v1/servers/{id}/console — стрим консоли + отправка команд
- Полная спецификация — docs/API.md

### Хранилище

- SQLite через modernc.org/sqlite (чистый Go, без CGO — критично для кросс-компиляции)
- Миграции — golang-migrate или встроенные, применяются при старте
- Таблицы: users, tokens, servers, backups, audit_log

### DNS & SSL (internal/dns)

Интерфейс DNSProvider:

```go
type DNSProvider interface {
    EnsureA(ctx context.Context, host, ip string) error
    EnsureSRV(ctx context.Context, host string, port int) error  // _minecraft._tcp, только Java
    SupportsSRV() bool
    Cleanup(ctx context.Context, host string) error
}
```

Реализации: deSEC (дефолт — умеет SRV), DuckDNS (без SRV), Cloudflare (свой домен).
Демон содержит мини-DynDNS: при смене внешнего IP обновляет записи.
SRV-записи создаются только для Java-серверов; Bedrock-клиенты SRV не читают —
для Bedrock панель старается выдать стандартный порт 19132/udp.

Сертификаты: autocert (ACME HTTP-01) по умолчанию; DNS-01 через Cloudflare-токен,
если 80-й порт недоступен. Fallback — self-signed с предупреждением в UI.

Режимы установщика:
1. Простой — бесплатный поддомен (deSEC/DuckDNS, запрашивается токен сервиса)
2. Расширенный — свой домен (инструкция по A-записи + проверка, или Cloudflare-токен — тогда всё само)
3. Без домена — доступ по IP, self-signed

### Bedrock-поддержка

Два пути:
1. **Geyser + Floodgate** на Java-сервере — галочка «кроссплей» при создании сервера:
   панель ставит их из каталога, открывает UDP 19132, прописывает конфиг
2. **Нативные Bedrock-ядра** (BDS, PocketMine-MP, PowerNukkitX) — отдельные CoreProvider;
   BDS — нативный процесс (не Java), всегда ProcessRunner-путь либо свой Docker-образ,
   UDP-протокол, отдельная логика graceful stop

### Прокси-серверы

Velocity/BungeeCord/Waterfall — тип `proxy`: без миров и игроков, консоль и файлы те же.
В UI страница прокси предлагает привязать существующие серверы панели
(автогенерация секций в velocity.toml / config.yml с forwarding-секретом).

### Веб-панель (web/)

- SPA: React + Vite + Tailwind
- Собирается в web/dist, встраивается в бинарник через go:embed
- Ключевые страницы: логин, список серверов, страница сервера
  (консоль, файлы, настройки, плагины/моды, бэкапы), админка (пользователи, токены)

#### Темы

Панель поддерживает произвольное число тем, а не только тёмную/светлую пару.

**Слой токенов.** Единственный источник цвета — CSS-переменные на `:root`.
Базовый набор (`web/src/themes/tokens.css` — объявляет контракт и дефолты):

```
--bg, --bg-elevated, --bg-inset      поверхности: фон страницы, карточки, «утопленные» блоки
--text, --text-muted, --text-faint   текст: основной, вторичный, третичный
--accent, --accent-hover, --accent-fg акцент и читаемый цвет текста НА акценте
--success, --warning, --danger, --info семантика (+ парные --*-bg для подложек)
--border, --border-strong            границы
--radius, --radius-sm, --radius-lg   скругления (нужны редактору кастомных тем)
--console-bg, --console-text,        консоль: подсветка уровней лога
--console-error, --console-warn,
--console-info, --console-debug,
--console-timestamp
```

Tailwind настраивается так, чтобы его цветовые утилиты резолвились в эти переменные
(`theme.extend.colors.bg = 'var(--bg)'` и т.д.). Компоненты не содержат цветов —
запрет проверяется ESLint-правилом на литералы цвета в JSX/CSS и падает в CI.

**Тема = один файл** `web/src/themes/<name>.css`, переопределяющий переменные.
Файл темы не содержит селекторов компонентов. Реестр тем (`themes/index.ts`)
собирается из каталога и отдаёт метаданные: id, отображаемое имя, `base: dark|light`
(нужен для тумблера в шапке и для редактора кастомных тем), превью-цвета.

Встроенные темы:

| id | Описание | base |
|---|---|---|
| `dark` | нейтральная тёмная, серо-графитовая, зелёный акцент (дефолт тёмной) | dark |
| `light` | светлая пара к `dark` | light |
| `midnight` | глубокая сине-чёрная, чистый чёрный фон (OLED-friendly) | dark |
| `grass` | тёмная с зелёно-земляными акцентами — сдержанная отсылка к игре, без пиксель-арта | dark |
| `nether` | тёмная с тёплыми красно-оранжевыми акцентами | dark |

**Контраст.** Каждая тема обязана проходить WCAG AA: 4.5:1 для обычного текста,
3:1 для крупного текста, границ инпутов и иконок-контролов. Проверка —
автотест, который парсит все файлы тем, считает контраст для фиксированного
списка пар (text/bg, text-muted/bg, accent-fg/accent, console-error/console-bg, …)
и падает на нарушении. Новая тема без прохождения теста не мержится.

**Хранение выбора.** Тема живёт в профиле пользователя на сервере
(`GET/PATCH /users/me`, поле `theme`), поэтому переезжает между браузерами.
Локально дублируется в localStorage как кэш. Значение `system` (дефолт для новых
пользователей) означает следование `prefers-color-scheme` с парой `dark`/`light`.

**Порядок применения (без FOUC).** Инлайн-скрипт в `index.html` до первого рендера
читает кэш из localStorage (или `prefers-color-scheme`, если кэша нет) и ставит
`data-theme` на `<html>`. React-приложение стартует уже в правильной теме;
после загрузки профиля значение с сервера сверяется с кэшем и при расхождении
применяется и перезаписывает кэш. Смена темы — только замена `data-theme`,
без перезагрузки и без промежуточного «белого кадра».

**Кастомные темы.** Редактор в настройках профиля: пользователь выбирает базу
(`dark` или `light`), меняет акцентный цвет и радиус скруглений простыми контролами
и видит живое превью. Результат — оверрайд-набор переменных поверх базы,
хранится в профиле. Формат обмена — JSON:

```json
{
  "schema": "mirocraft.theme/v1",
  "name": "My Theme",
  "base": "dark",
  "vars": { "--accent": "#7c5cff", "--accent-hover": "#6a4ce0", "--radius": "12px" }
}
```

Импорт валидируется: разрешён только whitelist имён переменных из контракта токенов,
значения — только цвета и длины (никаких `url()`, `expression`, произвольного CSS),
и импортируемая тема прогоняется через ту же проверку контраста — при провале
показывается предупреждение с конкретными парами, не прошедшими AA.

### Боты (bots/)

- Отдельные процессы, конфигурируются токеном панели + токеном бота
- Discord: discordgo, слэш-команды: /servers, /start, /stop, /status, /console (last N lines), /cmd
- Telegram: go-telegram-bot-api, те же команды + inline-кнопки
- Общий пакет bots/internal/panelclient — типизированный клиент REST API

### Установщики (installer/)

- install.sh (Ubuntu 22.04+/Debian): проверяет и ставит Docker при необходимости,
  скачивает бинарник под арх, создаёт systemd-юнит, генерит admin-пароль, печатает URL
- install.ps1 (Windows Server 2019+): скачивает бинарник, регистрирует Windows Service,
  открывает порт в фаерволе, генерит admin-пароль
- Оба идемпотентны: повторный запуск = обновление

## Безопасность (минимум для MVP)

- bcrypt для паролей, хэши для токенов
- Файловый менеджер: канонизация путей, запрет выхода за root сервера
- Rate limiting на логин
- HTTPS: автополучение сертификата через встроенный ACME (autocert) при наличии домена,
  иначе self-signed + предупреждение
