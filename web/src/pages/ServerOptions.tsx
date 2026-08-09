import { useEffect, useState } from "react";

import * as api from "../lib/api";

interface Props {
  server: api.Server;
  onSaved: (server: api.Server) => void;
}

/**
 * The server's own settings, as opposed to server.properties.
 *
 * These are the panel's: how much memory the JVM gets, which port it binds,
 * what flags it starts with. Changing them touches the launch command rather
 * than a file the game reads, which is why they live apart from the properties
 * editor even though both are "settings" to an operator.
 */
export function ServerOptions({ server, onSaved }: Props) {
  const [draft, setDraft] = useState({
    name: server.name,
    ram_mb: server.ram_mb,
    port: server.port,
    java_args: server.java_args,
    auto_start: server.auto_start,
    auto_restart: server.auto_restart,
    proxy_id: server.proxy_id ?? "",
  });

  // The proxies this account owns, for the picker below. Read once: the list
  // changes when someone creates a proxy, not while a form is open.
  const [proxies, setProxies] = useState<api.Server[]>([]);
  useEffect(() => {
    void api
      .listServers()
      .then(async (servers) => {
        const cores = await api.listCores();
        const proxyCores = new Set(cores.filter((c) => c.kind === "proxy").map((c) => c.id));
        setProxies(servers.filter((s) => proxyCores.has(s.core) && s.id !== server.id));
      })
      .catch(() => setProxies([]));
  }, [server.id]);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  // The parent polls the server every few seconds; without this the form would
  // be rebuilt from under the operator mid-edit.
  useEffect(() => {
    setDraft((current) => (current.name === "" ? { ...current, name: server.name } : current));
  }, [server.name]);

  const changed =
    draft.name !== server.name ||
    draft.ram_mb !== server.ram_mb ||
    draft.port !== server.port ||
    draft.java_args !== server.java_args ||
    draft.auto_start !== server.auto_start ||
    draft.auto_restart !== server.auto_restart ||
    draft.proxy_id !== (server.proxy_id ?? "");

  const running = server.status === "running" || server.status === "starting";

  async function handleSave() {
    setSaving(true);
    setError(null);
    setNotice(null);
    try {
      const saved = await api.patchServer(server.id, draft);
      onSaved(saved);
      setNotice(
        running
          ? "Сохранено. Сервер запущен — новые параметры применятся при перезапуске."
          : "Сохранено.",
      );
    } catch (err) {
      setError(err instanceof api.ApiError ? err.message : "Не удалось сохранить");
    } finally {
      setSaving(false);
    }
  }

  return (
    <section className="card p-4">
      {error && (
        <p className="mb-3 rounded-sm border border-danger bg-danger-bg px-3 py-2 text-sm text-danger">
          {error}
        </p>
      )}
      {notice && !error && <p className="mb-3 text-sm text-success">{notice}</p>}

      <div className="grid gap-3">
        <Field label="Название" htmlFor="opt-name">
          <input
            id="opt-name"
            value={draft.name}
            onChange={(e) => setDraft({ ...draft, name: e.target.value })}
            className="field h-9 w-72 text-sm"
          />
        </Field>

        <Field label="Память, МБ" htmlFor="opt-ram" hint="Столько получит JVM: -Xms и -Xmx">
          <input
            id="opt-ram"
            type="number"
            min={512}
            step={256}
            value={draft.ram_mb}
            onChange={(e) => setDraft({ ...draft, ram_mb: Number(e.target.value) })}
            className="field h-9 w-32 text-sm"
          />
        </Field>

        <Field label="Порт" htmlFor="opt-port" hint="Панель следит, чтобы он не совпал с чужим">
          <input
            id="opt-port"
            type="number"
            min={1}
            max={65535}
            value={draft.port}
            onChange={(e) => setDraft({ ...draft, port: Number(e.target.value) })}
            className="field h-9 w-32 text-sm"
          />
        </Field>

        <Field
          label="Флаги JVM"
          htmlFor="opt-java"
          hint="Через пробел. Кодировку и -Xmx панель ставит сама — дублировать не нужно"
        >
          <input
            id="opt-java"
            value={draft.java_args}
            onChange={(e) => setDraft({ ...draft, java_args: e.target.value })}
            placeholder="-XX:+UseG1GC -XX:MaxGCPauseMillis=200"
            className="field h-9 w-full font-mono text-sm"
          />
        </Field>

        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={draft.auto_start}
            onChange={(e) => setDraft({ ...draft, auto_start: e.target.checked })}
          />
          Запускать вместе с панелью
        </label>

        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={draft.auto_restart}
            onChange={(e) => setDraft({ ...draft, auto_restart: e.target.checked })}
          />
          Поднимать после падения
        </label>

        {proxies.length > 0 && (
          <label className="grid gap-1">
            <span className="text-sm text-muted">За каким прокси</span>
            <select
              className="field w-64"
              value={draft.proxy_id}
              onChange={(e) => setDraft({ ...draft, proxy_id: e.target.value })}
            >
              <option value="">напрямую, без прокси</option>
              {proxies.map((proxy) => (
                <option key={proxy.id} value={proxy.id}>
                  {proxy.name}
                </option>
              ))}
            </select>
            <span className="text-xs text-muted">
              Панель сама пропишет сервер в конфигурацию прокси и выключит здесь online-mode:
              игрока проверяет прокси, и вторая проверка не проходит.
            </span>
          </label>
        )}

        <div className="flex items-center gap-3">
          <button
            type="button"
            className="btn btn-primary"
            disabled={!changed || saving}
            onClick={() => void handleSave()}
          >
            {saving ? "Сохранение…" : "Сохранить"}
          </button>
          <button
            type="button"
            className="btn btn-ghost"
            disabled={!changed || saving}
            onClick={() =>
              setDraft({
                name: server.name,
                ram_mb: server.ram_mb,
                port: server.port,
                java_args: server.java_args,
                auto_start: server.auto_start,
                auto_restart: server.auto_restart,
                proxy_id: server.proxy_id ?? "",
              })
            }
          >
            Сбросить
          </button>
        </div>
      </div>
    </section>
  );
}

function Field({
  label,
  htmlFor,
  hint,
  children,
}: {
  label: string;
  htmlFor: string;
  hint?: string;
  children: React.ReactNode;
}) {
  return (
    <div className="grid gap-1 sm:grid-cols-[10rem_1fr] sm:items-center sm:gap-4">
      <div>
        <label htmlFor={htmlFor} className="text-sm">
          {label}
        </label>
        {hint && <p className="text-xs text-muted">{hint}</p>}
      </div>
      {children}
    </div>
  );
}
