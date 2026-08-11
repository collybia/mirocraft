import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { useNavigate, useParams } from "react-router-dom";

import * as api from "../lib/api";
import { ServerBackups } from "./ServerBackups";
import { ServerCatalog } from "./ServerCatalog";
import { ServerConnect } from "./ServerConnect";
import { ServerFiles } from "./ServerFiles";
import { ServerModpacks } from "./ServerModpacks";
import { ServerOptions } from "./ServerOptions";
import { ServerSettings } from "./ServerSettings";
import { StatusBadge } from "./Servers";

type Tab =
  | "console"
  | "connect"
  | "files"
  | "settings"
  | "catalog"
  | "modpacks"
  | "backups"
  | "options";

const TABS: { id: Tab; label: string }[] = [
  { id: "console", label: "Консоль" },
  { id: "connect", label: "Подключение" },
  { id: "files", label: "Файлы" },
  { id: "settings", label: "server.properties" },
  { id: "catalog", label: "Дополнения" },
  { id: "modpacks", label: "Модпаки" },
  { id: "backups", label: "Бэкапы" },
  { id: "options", label: "Параметры" },
];

/** How many lines the console keeps in the DOM. */
const MAX_RENDERED_LINES = 1000;

interface Frame {
  key: number;
  ts: string;
  stream: string;
  text: string;
}

export function ServerDetail() {
  const { id = "" } = useParams();
  const navigate = useNavigate();

  const [server, setServer] = useState<api.Server | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [lines, setLines] = useState<Frame[]>([]);
  const [connected, setConnected] = useState(false);
  const [command, setCommand] = useState("");
  const [tab, setTab] = useState<Tab>("console");

  const socketRef = useRef<WebSocket | null>(null);
  const bottomRef = useRef<HTMLDivElement | null>(null);
  const keyRef = useRef(0);
  const pinnedRef = useRef(true);

  const reload = useCallback(async () => {
    try {
      setServer(await api.getServer(id));
    } catch (err) {
      setError(err instanceof Error ? err.message : "Сервер не найден");
    }
  }, [id]);

  useEffect(() => {
    void reload();
    const timer = setInterval(() => void reload(), 5000);
    return () => clearInterval(timer);
  }, [reload]);

  // A stopped server has no console to attach to, so the socket is opened
  // only while one could exist — and re-opened when the server starts, which
  // is why this drives the effect below rather than mounting once.
  const consoleAvailable =
    server?.status === "running" ||
    server?.status === "starting" ||
    server?.status === "stopping";

  useEffect(() => {
    if (!consoleAvailable) {
      setConnected(false);
      return;
    }

    let closed = false;
    let socket: WebSocket | null = null;

    const push = (frame: Omit<Frame, "key">) => {
      setLines((current) => {
        const next = [...current, { ...frame, key: keyRef.current++ }];
        return next.length > MAX_RENDERED_LINES
          ? next.slice(next.length - MAX_RENDERED_LINES)
          : next;
      });
    };

    async function connect() {
      try {
        socket = await api.consoleSocket(id);
      } catch {
        // The server stopped between the status poll and the ticket request.
        // The badge already says so; a red line in the console would only add
        // noise.
        return;
      }
      if (closed) {
        socket.close();
        return;
      }

      socketRef.current = socket;
      socket.onopen = () => setConnected(true);
      socket.onclose = () => setConnected(false);

      socket.onmessage = (event) => {
        const frame = JSON.parse(event.data as string);
        switch (frame.type) {
          case "line":
            push({ ts: frame.ts, stream: frame.stream, text: frame.text });
            break;
          case "status":
            setServer((current) => (current ? { ...current, status: frame.status } : current));
            break;
          case "error":
            // A closing frame about the server not running is expected when
            // it stops; only genuine rejections are worth showing.
            if (frame.code !== "server_not_found" && frame.code !== "server_not_running") {
              push({ ts: new Date().toISOString(), stream: "stderr", text: `! ${frame.message}` });
            }
            break;
        }
      };
    }

    void connect();

    return () => {
      closed = true;
      socketRef.current?.close();
      socketRef.current = null;
    };
  }, [id, consoleAvailable]);

  // Follow the tail only while the reader is already at the bottom; yanking
  // the view down while someone scrolls back through a crash is maddening.
  useEffect(() => {
    if (pinnedRef.current) bottomRef.current?.scrollIntoView({ block: "end" });
  }, [lines]);

  async function handlePower(action: "start" | "stop" | "restart" | "kill") {
    setError(null);
    try {
      await api.power(id, action);
      // The action is asynchronous; the status arrives over the socket or the
      // next poll, so nothing is assumed here.
    } catch (err) {
      setError(err instanceof api.ApiError ? err.message : "Команда не выполнена");
    }
  }

  async function handleCommand(event: FormEvent) {
    event.preventDefault();
    const text = command.trim();
    if (!text) return;

    const socket = socketRef.current;
    if (socket && socket.readyState === WebSocket.OPEN) {
      socket.send(JSON.stringify({ type: "command", text }));
    } else {
      try {
        await api.sendCommand(id, text);
      } catch (err) {
        setError(err instanceof api.ApiError ? err.message : "Команда не отправлена");
        return;
      }
    }
    setCommand("");
  }

  async function handleDelete() {
    if (!server) return;
    const typed = window.prompt(
      `Удаление уничтожит миры без возможности восстановления.\nВведите название сервера для подтверждения:`,
    );
    if (typed !== server.name) return;

    try {
      await api.deleteServer(id, server.name);
      navigate("/");
    } catch (err) {
      setError(err instanceof api.ApiError ? err.message : "Не удалось удалить сервер");
    }
  }

  if (!server) {
    return <p className="text-muted">{error ?? "Загрузка…"}</p>;
  }

  const running = server.status === "running" || server.status === "starting";

  return (
    <div>
      <div className="mb-4 flex flex-wrap items-center gap-3">
        <h1 className="text-lg font-semibold">{server.name}</h1>
        <StatusBadge status={server.status} />
        <span className="text-sm text-muted">
          {server.core} {server.version} · порт {server.port}
        </span>

        <div className="ml-auto flex gap-2">
          <button
            type="button"
            className="btn btn-primary"
            disabled={running}
            onClick={() => void handlePower("start")}
          >
            Запустить
          </button>
          <button
            type="button"
            className="btn btn-ghost"
            disabled={!running}
            onClick={() => void handlePower("restart")}
          >
            Рестарт
          </button>
          <button
            type="button"
            className="btn btn-ghost"
            disabled={!running}
            onClick={() => void handlePower("stop")}
          >
            Остановить
          </button>
          <button type="button" className="btn btn-danger" onClick={() => void handleDelete()}>
            Удалить
          </button>
        </div>
      </div>

      {error && (
        <p className="mb-4 rounded-sm border border-danger bg-danger-bg px-3 py-2 text-sm text-danger">
          {error}
        </p>
      )}

      {server.metrics && (
        <div className="mb-4 grid gap-3 sm:grid-cols-4">
          <Metric label="Память" value={`${server.metrics.ram_used_mb} МБ`} />
          <Metric label="CPU" value={`${server.metrics.cpu_percent.toFixed(1)} %`} />
          <Metric label="Аптайм" value={formatUptime(server.metrics.uptime_seconds)} />
          <Metric
            label="Игроки"
            value={
              server.metrics.players_online == null
                ? "—"
                : `${server.metrics.players_online}/${server.metrics.players_max}`
            }
          />
        </div>
      )}

      <nav className="mb-4 flex gap-1 border-b border-line text-sm">
        {TABS.map((item) => (
          <button
            key={item.id}
            type="button"
            onClick={() => setTab(item.id)}
            className={
              // -mb-px so the active tab's border sits on top of the nav's own
              // line rather than beside it.
              tab === item.id
                ? "-mb-px border-b-2 border-accent px-3 py-2 text-body"
                : "-mb-px border-b-2 border-transparent px-3 py-2 text-muted hover:text-body"
            }
          >
            {item.label}
          </button>
        ))}
      </nav>

      {tab === "connect" && <ServerConnect serverId={id} />}
      {tab === "files" && <ServerFiles serverId={id} />}
      {tab === "settings" && <ServerSettings serverId={id} />}
      {tab === "catalog" && <ServerCatalog serverId={id} />}
      {tab === "modpacks" && <ServerModpacks serverId={id} running={running} />}
      {tab === "backups" && <ServerBackups serverId={id} running={running} />}
      {tab === "options" && <ServerOptions server={server} onSaved={setServer} />}

      {/*
        The console stays mounted while other tabs are shown, only hidden: it
        holds a WebSocket and the scrollback, and unmounting would drop both,
        so glancing at the files would silently lose the log of a server that
        was mid-crash.
      */}
      <section className={`card overflow-hidden ${tab === "console" ? "" : "hidden"}`}>
        <header className="flex items-center gap-2 border-b border-line px-4 py-2 text-sm">
          <span className="text-muted">Консоль</span>
          <span className={connected ? "text-success" : "text-faint"}>
            {connected ? "● подключено" : consoleAvailable ? "○ подключение…" : "○ сервер остановлен"}
          </span>
        </header>

        <div
          className="h-96 overflow-y-auto bg-console-bg px-4 py-3 font-mono text-xs leading-relaxed"
          onScroll={(e) => {
            const el = e.currentTarget;
            pinnedRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
          }}
        >
          {lines.length === 0 ? (
            <p className="text-console-debug">
              {consoleAvailable
                ? "Ждём вывод сервера…"
                : "Сервер остановлен. Запустите его, чтобы увидеть лог."}
            </p>
          ) : (
            lines.map((line) => <ConsoleLine key={line.key} line={line} />)
          )}
          <div ref={bottomRef} />
        </div>

        <form onSubmit={handleCommand} className="flex gap-2 border-t border-line p-3">
          <input
            value={command}
            onChange={(e) => setCommand(e.target.value)}
            placeholder="Команда, например: say привет"
            maxLength={512}
            className="field font-mono"
          />
          <button type="submit" className="btn btn-primary">
            Отправить
          </button>
        </form>
      </section>
    </div>
  );
}

function ConsoleLine({ line }: { line: Frame }) {
  return (
    <div className="whitespace-pre-wrap break-words">
      <span className="text-console-timestamp">{formatTime(line.ts)} </span>
      <span className={levelClass(line)}>{line.text}</span>
    </div>
  );
}

/**
 * levelClass picks the colour token for a log line. Levels are recognised from
 * the text because a Minecraft server writes everything to stdout, including
 * errors; stderr alone would miss most of them.
 */
function levelClass(line: Frame): string {
  const text = line.text;
  if (line.stream === "stderr" || /\b(ERROR|SEVERE|FATAL)\b/.test(text)) return "text-console-error";
  if (/\bWARN(ING)?\b/.test(text)) return "text-console-warn";
  if (/\bDEBUG\b/.test(text)) return "text-console-debug";
  if (/\bINFO\b/.test(text)) return "text-console-info";
  return "text-console-text";
}

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <div className="card p-3">
      <div className="text-xs text-muted">{label}</div>
      <div className="text-lg text-body">{value}</div>
    </div>
  );
}

function formatTime(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "--:--:--";
  return date.toLocaleTimeString("ru-RU", { hour12: false });
}

function formatUptime(seconds: number): string {
  if (seconds < 60) return `${seconds} с`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes} мин`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} ч ${minutes % 60} мин`;
  return `${Math.floor(hours / 24)} д ${hours % 24} ч`;
}
