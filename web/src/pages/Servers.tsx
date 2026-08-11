import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type FormEvent,
} from "react";
import { Link } from "react-router-dom";

import * as api from "../lib/api";
import { PageHeader } from "../components/Layout";
import {
  CpuIcon,
  MemoryIcon,
  PlayIcon,
  PlayersIcon,
  PlusIcon,
  RestartIcon,
  ServersIcon,
  StopIcon,
} from "../components/Icon";

/** How many samples the CPU sparkline keeps. At a 5s poll, about two minutes. */
const HISTORY = 24;

export function Servers() {
  const [servers, setServers] = useState<api.Server[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);

  // CPU history per server, accumulated from the polls this page has already
  // made. Deliberately not persisted: a sparkline restored from storage would
  // claim to show a past the panel did not watch.
  const [history, setHistory] = useState<Record<string, number[]>>({});

  const reload = useCallback(async () => {
    try {
      const list = await api.listServers(true);
      setServers(list);
      setHistory((current) => {
        const next: Record<string, number[]> = {};
        for (const server of list) {
          const cpu = server.metrics?.cpu_percent ?? 0;
          const previous = current[server.id] ?? [];
          next[server.id] = [...previous, cpu].slice(-HISTORY);
        }
        return next;
      });
      setError(null);
    } catch (err) {
      setError(
        err instanceof Error ? err.message : "Не удалось загрузить серверы",
      );
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void reload();
    // Statuses change without the panel asking, so the list refreshes on a
    // timer until the events bus lands in task 2.7.
    const timer = setInterval(() => void reload(), 5000);
    return () => clearInterval(timer);
  }, [reload]);

  const running = servers.filter((s) => s.status === "running").length;

  return (
    <div>
      <PageHeader
        title="Серверы"
        description={
          servers.length > 0
            ? `${plural(servers.length, "сервер", "сервера", "серверов")} · ${running} работает`
            : undefined
        }
        actions={
          <button
            type="button"
            className={creating ? "btn btn-ghost" : "btn btn-primary"}
            onClick={() => setCreating((v) => !v)}
          >
            {creating ? (
              "Отмена"
            ) : (
              <>
                <PlusIcon />
                Создать сервер
              </>
            )}
          </button>
        }
      />

      {creating && (
        <CreateServerForm
          onCreated={() => {
            setCreating(false);
            void reload();
          }}
        />
      )}

      {error && (
        <p className="mb-4 rounded-sm border border-danger bg-danger-bg px-3 py-2 text-sm text-danger">
          {error}
        </p>
      )}

      {loading ? (
        <div className="grid gap-3 xl:grid-cols-2">
          <CardSkeleton />
          <CardSkeleton />
        </div>
      ) : servers.length === 0 ? (
        <EmptyState onCreate={() => setCreating(true)} />
      ) : (
        <ul className="grid gap-3 xl:grid-cols-2">
          {servers.map((server) => (
            <ServerCard
              key={server.id}
              server={server}
              history={history[server.id] ?? []}
              onChanged={() => void reload()}
            />
          ))}
        </ul>
      )}
    </div>
  );
}

function EmptyState({ onCreate }: { onCreate: () => void }) {
  return (
    <div className="card flex flex-col items-center px-6 py-12 text-center">
      <span className="mb-4 flex h-12 w-12 items-center justify-center rounded-full bg-accent-bg text-accent">
        <ServersIcon className="h-6 w-6" />
      </span>
      <p className="mb-1 text-base">Пока нет ни одного сервера</p>
      <p className="mb-5 max-w-sm text-sm text-muted">
        Выберите ядро и версию — панель скачает всё сама, включая Java. Это
        займёт меньше минуты.
      </p>
      <button type="button" className="btn btn-primary" onClick={onCreate}>
        <PlusIcon />
        Создать первый сервер
      </button>
    </div>
  );
}

function CardSkeleton() {
  return (
    <li className="card p-4">
      <div className="skeleton mb-3 h-5 w-40" />
      <div className="skeleton mb-2 h-4 w-64" />
      <div className="skeleton h-9 w-full" />
    </li>
  );
}

/**
 * A server card that says what the server is doing and lets you act on it.
 *
 * The old one listed core, RAM and port — the three things that never change
 * — and offered no action at all, so seeing whether anyone was online meant
 * opening the server, and starting it meant opening it too. What belongs here
 * is what moves: players, load, and the buttons.
 */
function ServerCard({
  server,
  history,
  onChanged,
}: {
  server: api.Server;
  history: number[];
  onChanged: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const running = server.status === "running";
  const transitioning =
    server.status === "starting" || server.status === "stopping";
  const metrics = server.metrics;

  async function power(action: "start" | "stop" | "restart") {
    setBusy(true);
    setError(null);
    try {
      await api.power(server.id, action);
      onChanged();
    } catch (err) {
      setError(
        err instanceof api.ApiError ? err.message : "Команда не выполнена",
      );
    } finally {
      setBusy(false);
    }
  }

  return (
    <li className="card card-interactive flex flex-col p-4">
      <div className="flex items-start gap-3">
        <div className="min-w-0 flex-1">
          <Link
            to={`/servers/${server.id}`}
            className="text-base font-medium text-body hover:text-accent"
          >
            {server.name}
          </Link>
          <p className="mt-0.5 truncate text-sm text-muted">
            {server.core} {server.version} · порт {server.port}
          </p>
        </div>
        <StatusBadge status={server.status} />
      </div>

      <div className="mt-3 flex flex-wrap items-end justify-between gap-3">
        {/*
          A stopped server has no players and no load, and printing "— игроков
          — CPU" fills the card with the absence of information. What is still
          true about it is how much memory it will take, so that is all it says.
        */}
        <dl className="flex flex-wrap items-center gap-x-5 gap-y-2 text-sm">
          {running && (
            <>
              <Metric
                icon={<PlayersIcon />}
                label="игроков"
                value={
                  metrics?.players_online != null
                    ? `${metrics.players_online}/${metrics.players_max}`
                    : "—"
                }
              />
              <Metric
                icon={<CpuIcon />}
                label="CPU"
                value={metrics ? `${metrics.cpu_percent.toFixed(0)}%` : "—"}
              />
            </>
          )}
          <Metric
            icon={<MemoryIcon />}
            label={running ? "памяти" : "выделено"}
            value={
              running && metrics
                ? `${formatGb(metrics.ram_used_mb)}/${formatGb(
                    metrics.ram_limit_mb || server.ram_mb,
                  )} ГБ`
                : `${formatGb(server.ram_mb)} ГБ`
            }
          />
        </dl>

        <div className="flex items-center gap-1">
          {running && <Sparkline values={history} />}
          {running || transitioning ? (
            <>
              <button
                type="button"
                className="btn btn-quiet btn-icon"
                title="Перезапустить"
                disabled={busy || transitioning}
                onClick={() => void power("restart")}
              >
                <RestartIcon />
              </button>
              <button
                type="button"
                className="btn btn-quiet btn-icon"
                title="Остановить"
                disabled={busy || transitioning}
                onClick={() => void power("stop")}
              >
                <StopIcon />
              </button>
            </>
          ) : (
            <button
              type="button"
              className="btn btn-ghost btn-sm"
              disabled={busy}
              onClick={() => void power("start")}
            >
              <PlayIcon />
              Запустить
            </button>
          )}
        </div>
      </div>

      {error && <p className="mt-2 text-xs text-danger">{error}</p>}
    </li>
  );
}

function Metric({
  icon,
  label,
  value,
}: {
  icon: React.ReactNode;
  label: string;
  value: string;
}) {
  return (
    <div className="flex items-center gap-1.5">
      <span className="text-faint">{icon}</span>
      <dd className="tabular-nums">{value}</dd>
      <dt className="text-xs text-faint">{label}</dt>
    </div>
  );
}

/**
 * A CPU trace over the samples this page has seen.
 *
 * Scaled to a fixed 100% rather than to its own maximum: a server idling
 * between 1% and 3% would otherwise draw the same dramatic mountain range as
 * one that is struggling, which is the kind of chart that lies without
 * containing a single wrong number.
 */
function Sparkline({ values }: { values: number[] }) {
  if (values.length < 2) return null;

  const width = 60;
  const height = 20;
  const step = width / (values.length - 1);
  const points = values
    .map((value, i) => {
      const y = height - (Math.min(value, 100) / 100) * height;
      return `${(i * step).toFixed(1)},${y.toFixed(1)}`;
    })
    .join(" ");

  return (
    <svg
      width={width}
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      className="mr-1 text-accent"
      aria-hidden="true"
    >
      <polyline
        points={points}
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinejoin="round"
        strokeLinecap="round"
      />
    </svg>
  );
}

export function StatusBadge({ status }: { status: api.ServerStatus }) {
  const styles: Record<api.ServerStatus, string> = {
    running: "bg-success-bg text-success",
    starting: "bg-info-bg text-info",
    stopping: "bg-warning-bg text-warning",
    stopped: "bg-hover text-muted",
    crashed: "bg-danger-bg text-danger",
    creating: "bg-info-bg text-info",
  };

  const dots: Record<api.ServerStatus, string> = {
    running: "bg-success dot-live",
    starting: "bg-info dot-live",
    stopping: "bg-warning dot-live",
    stopped: "bg-faint",
    crashed: "bg-danger",
    creating: "bg-info dot-live",
  };

  const labels: Record<api.ServerStatus, string> = {
    running: "работает",
    starting: "запускается",
    stopping: "останавливается",
    stopped: "остановлен",
    crashed: "упал",
    creating: "создаётся",
  };

  return (
    <span className={`badge ${styles[status]}`}>
      <span className={`dot ${dots[status]}`} />
      {labels[status]}
    </span>
  );
}

function formatGb(mb: number): string {
  return (mb / 1024).toFixed(1);
}

/** Russian counts need three forms, and "3 сервера" is worth getting right. */
function plural(n: number, one: string, few: string, many: string): string {
  const mod100 = n % 100;
  const mod10 = n % 10;
  if (mod100 >= 11 && mod100 <= 14) return `${n} ${many}`;
  if (mod10 === 1) return `${n} ${one}`;
  if (mod10 >= 2 && mod10 <= 4) return `${n} ${few}`;
  return `${n} ${many}`;
}

function CreateServerForm({ onCreated }: { onCreated: () => void }) {
  const [name, setName] = useState("");
  const [core, setCore] = useState("paper");
  const [version, setVersion] = useState("");
  const [ram, setRam] = useState(2048);
  const [eula, setEula] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const firstField = useRef<HTMLInputElement | null>(null);

  // The cores and their versions come from the daemon, not from a list kept
  // here. A list kept here drifts: this one offered forge and neoforge before
  // either existed, and picking one failed at provisioning time.
  const [cores, setCores] = useState<api.Core[]>([]);
  const [versions, setVersions] = useState<api.CoreVersion[]>([]);
  const [versionsError, setVersionsError] = useState<string | null>(null);

  useEffect(() => {
    firstField.current?.focus();
    void api
      .listCores()
      .then((list) => {
        setCores(list);
        if (list.length > 0 && !list.some((c) => c.id === core))
          setCore(list[0].id);
      })
      .catch(() => setCores([]));
    // Once: the set of cores changes when the daemon is upgraded, not while a
    // form is open.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    if (!core) return;

    let cancelled = false;
    setVersions([]);
    setVersionsError(null);

    void api
      .listCoreVersions(core)
      .then((list) => {
        if (cancelled) return;
        setVersions(list);
        // The newest release, which is what someone picking a core wants -
        // not the newest snapshot, which sorts first and is not for players.
        const release = list.find((v) => v.channel === "release") ?? list[0];
        if (release) setVersion(release.id);
      })
      .catch((err) => {
        if (cancelled) return;
        // The list comes from upstream, which can be down. Typing a version by
        // hand still works, so this is a notice rather than a blocked form.
        setVersionsError(
          err instanceof api.ApiError
            ? err.message
            : "Не удалось получить список версий",
        );
      });

    return () => {
      cancelled = true;
    };
  }, [core]);

  async function handleSubmit(event: FormEvent) {
    event.preventDefault();
    setError(null);
    setBusy(true);

    try {
      await api.createServer({
        name,
        core,
        version,
        ram_mb: ram,
        eula_accepted: eula,
      });
      onCreated();
    } catch (err) {
      setError(
        err instanceof api.ApiError ? err.message : "Не удалось создать сервер",
      );
    } finally {
      setBusy(false);
    }
  }

  return (
    <form onSubmit={handleSubmit} className="card mb-5 p-5">
      <div className="grid gap-4 sm:grid-cols-2">
        <div>
          <label className="mb-1.5 block text-sm text-muted" htmlFor="name">
            Название
          </label>
          <input
            id="name"
            ref={firstField}
            required
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="Выживание"
            className="field"
          />
        </div>

        <div>
          <label className="mb-1.5 block text-sm text-muted" htmlFor="core">
            Ядро
          </label>
          <select
            id="core"
            value={core}
            onChange={(e) => setCore(e.target.value)}
            className="field"
          >
            {cores.length === 0 && <option value={core}>{core}</option>}
            {cores.map((c) => (
              <option key={c.id} value={c.id}>
                {c.name}
              </option>
            ))}
          </select>
        </div>

        <div>
          <label className="mb-1.5 block text-sm text-muted" htmlFor="version">
            Версия
          </label>
          {versions.length > 0 ? (
            <select
              id="version"
              value={version}
              onChange={(e) => setVersion(e.target.value)}
              className="field"
            >
              {versions.map((v) => (
                <option key={v.id} value={v.id}>
                  {v.id}
                  {v.channel === "snapshot" ? " (снапшот)" : ""}
                </option>
              ))}
            </select>
          ) : (
            <input
              id="version"
              required
              value={version}
              onChange={(e) => setVersion(e.target.value)}
              placeholder="1.21.4"
              className="field"
            />
          )}
          {versionsError && (
            <p className="mt-1 text-xs text-warning">{versionsError}</p>
          )}
        </div>

        <div>
          <label className="mb-1.5 block text-sm text-muted" htmlFor="ram">
            Память, МБ
          </label>
          <input
            id="ram"
            type="number"
            min={512}
            step={512}
            value={ram}
            onChange={(e) => setRam(Number(e.target.value))}
            className="field tabular-nums"
          />
        </div>

        {cores.find((c) => c.id === core)?.builds_locally && (
          <p className="rounded-sm border border-warning bg-warning-bg px-3 py-2 text-xs text-warning sm:col-span-2">
            Это ядро не раздаётся готовым — панель соберёт его на этой машине.
            Первый сервер такой версии запустится минут через пятнадцать,
            следующие — сразу.
          </p>
        )}

        <label className="flex items-start gap-2.5 text-sm text-muted sm:col-span-2">
          <input
            type="checkbox"
            checked={eula}
            onChange={(e) => setEula(e.target.checked)}
            className="mt-0.5 accent-accent"
          />
          <span>
            Я принимаю{" "}
            <a
              href="https://www.minecraft.net/eula"
              target="_blank"
              rel="noreferrer"
              className="text-accent underline"
            >
              EULA Mojang
            </a>
            . Без этого сервер не запустится.
          </span>
        </label>

        {error && (
          <p className="rounded-sm border border-danger bg-danger-bg px-3 py-2 text-sm text-danger sm:col-span-2">
            {error}
          </p>
        )}
      </div>

      <div className="mt-5 border-t border-line pt-4">
        <button
          type="submit"
          disabled={busy || !eula}
          className="btn btn-primary"
        >
          {busy ? "Создание…" : "Создать"}
        </button>
      </div>
    </form>
  );
}
