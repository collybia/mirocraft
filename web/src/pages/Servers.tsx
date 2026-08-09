import { useCallback, useEffect, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";

import * as api from "../lib/api";

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
  const [version, setVersion] = useState("");
  const [ram, setRam] = useState(2048);
  const [eula, setEula] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  // The cores and their versions come from the daemon, not from a list kept
  // here. A list kept here drifts: this one offered forge and neoforge before
  // either existed, and picking one failed at provisioning time.
  const [cores, setCores] = useState<api.Core[]>([]);
  const [versions, setVersions] = useState<api.CoreVersion[]>([]);
  const [versionsError, setVersionsError] = useState<string | null>(null);

  useEffect(() => {
    void api
      .listCores()
      .then((list) => {
        setCores(list);
        if (list.length > 0 && !list.some((c) => c.id === core)) setCore(list[0].id);
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
          err instanceof api.ApiError ? err.message : "Не удалось получить список версий",
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
          {cores.length === 0 && <option value={core}>{core}</option>}
          {cores.map((c) => (
            <option key={c.id} value={c.id}>
              {c.name}
            </option>
          ))}
        </select>
      </div>

      <div>
        <label className="mb-1 block text-sm text-muted" htmlFor="version">
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
        {versionsError && <p className="mt-1 text-xs text-warning">{versionsError}</p>}
      </div>

      {cores.find((c) => c.id === core)?.builds_locally && (
        <p className="text-xs text-warning sm:col-span-2">
          Это ядро не раздаётся готовым — панель соберёт его на этой машине. Первый сервер такой
          версии запустится минут через пятнадцать, следующие — сразу.
        </p>
      )}

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
