import { useCallback, useEffect, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";

import * as api from "../lib/api";

const CORES = ["paper", "vanilla", "purpur", "fabric", "forge", "neoforge"];

export function Servers() {
  const [servers, setServers] = useState<api.Server[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);

  const reload = useCallback(async () => {
    try {
      setServers(await api.listServers());
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Не удалось загрузить серверы");
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

  return (
    <div>
      <div className="mb-5 flex items-center justify-between">
        <h1 className="text-lg font-semibold">Серверы</h1>
        <button type="button" className="btn btn-primary" onClick={() => setCreating((v) => !v)}>
          {creating ? "Отмена" : "Создать сервер"}
        </button>
      </div>

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
        <p className="text-muted">Загрузка…</p>
      ) : servers.length === 0 ? (
        <div className="card p-8 text-center">
          <p className="mb-1 text-body">Пока нет ни одного сервера</p>
          <p className="text-sm text-muted">Создайте первый — это займёт меньше минуты.</p>
        </div>
      ) : (
        <ul className="grid gap-3 sm:grid-cols-2">
          {servers.map((server) => (
            <ServerCard key={server.id} server={server} />
          ))}
        </ul>
      )}
    </div>
  );
}

function ServerCard({ server }: { server: api.Server }) {
  return (
    <li className="card p-4">
      <div className="mb-2 flex items-start justify-between gap-3">
        <Link to={`/servers/${server.id}`} className="font-medium text-body hover:text-accent">
          {server.name}
        </Link>
        <StatusBadge status={server.status} />
      </div>

      <dl className="grid grid-cols-2 gap-x-4 gap-y-1 text-sm text-muted">
        <div className="flex gap-2">
          <dt>Ядро</dt>
          <dd className="text-body">
            {server.core} {server.version}
          </dd>
        </div>
        <div className="flex gap-2">
          <dt>Порт</dt>
          <dd className="text-body">{server.port}</dd>
        </div>
        <div className="flex gap-2">
          <dt>RAM</dt>
          <dd className="text-body">{server.ram_mb} МБ</dd>
        </div>
        {server.metrics?.players_online != null && (
          <div className="flex gap-2">
            <dt>Игроки</dt>
            <dd className="text-body">
              {server.metrics.players_online}/{server.metrics.players_max}
            </dd>
          </div>
        )}
      </dl>
    </li>
  );
}

export function StatusBadge({ status }: { status: api.ServerStatus }) {
  const styles: Record<api.ServerStatus, string> = {
    running: "border-success bg-success-bg text-success",
    starting: "border-info bg-info-bg text-info",
    stopping: "border-warning bg-warning-bg text-warning",
    stopped: "border-line-strong text-muted",
    crashed: "border-danger bg-danger-bg text-danger",
    creating: "border-info bg-info-bg text-info",
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
    <span className={`rounded-full border px-2 py-0.5 text-xs ${styles[status]}`}>
      {labels[status]}
    </span>
  );
}

function CreateServerForm({ onCreated }: { onCreated: () => void }) {
  const [name, setName] = useState("");
  const [core, setCore] = useState("paper");
  const [version, setVersion] = useState("1.21.4");
  const [ram, setRam] = useState(2048);
  const [eula, setEula] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

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
      setError(err instanceof api.ApiError ? err.message : "Не удалось создать сервер");
    } finally {
      setBusy(false);
    }
  }

  return (
    <form onSubmit={handleSubmit} className="card mb-5 grid gap-4 p-4 sm:grid-cols-2">
      <div>
        <label className="mb-1 block text-sm text-muted" htmlFor="name">
          Название
        </label>
        <input
          id="name"
          required
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="survival"
          className="field"
        />
      </div>

      <div>
        <label className="mb-1 block text-sm text-muted" htmlFor="core">
          Ядро
        </label>
        <select id="core" value={core} onChange={(e) => setCore(e.target.value)} className="field">
          {CORES.map((c) => (
            <option key={c} value={c}>
              {c}
            </option>
          ))}
        </select>
      </div>

      <div>
        <label className="mb-1 block text-sm text-muted" htmlFor="version">
          Версия
        </label>
        <input
          id="version"
          required
          value={version}
          onChange={(e) => setVersion(e.target.value)}
          className="field"
        />
      </div>

      <div>
        <label className="mb-1 block text-sm text-muted" htmlFor="ram">
          Память, МБ
        </label>
        <input
          id="ram"
          type="number"
          min={512}
          step={512}
          value={ram}
          onChange={(e) => setRam(Number(e.target.value))}
          className="field"
        />
      </div>

      <label className="flex items-start gap-2 text-sm text-muted sm:col-span-2">
        <input
          type="checkbox"
          checked={eula}
          onChange={(e) => setEula(e.target.checked)}
          className="mt-0.5"
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

      <div className="sm:col-span-2">
        <button type="submit" disabled={busy || !eula} className="btn btn-primary">
          {busy ? "Создание…" : "Создать"}
        </button>
      </div>
    </form>
  );
}
