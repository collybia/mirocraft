import { useCallback, useEffect, useMemo, useState } from "react";

import * as api from "../lib/api";

interface Props {
  serverId: string;
}

/**
 * Editor for server.properties.
 *
 * Two lists rather than one: keys the panel knows get a control shaped like
 * their type and a note explaining them, and everything else gets a text box.
 * A modded or forked server invents keys freely, and hiding what the schema
 * does not recognise would lose them the first time anyone pressed save.
 */
export function ServerSettings({ serverId }: Props) {
  const [settings, setSettings] = useState<api.Settings | null>(null);
  const [draft, setDraft] = useState<Record<string, string>>({});
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [filter, setFilter] = useState("");

  const load = useCallback(async () => {
    setError(null);
    try {
      const loaded = await api.getSettings(serverId);
      setSettings(loaded);
      setDraft({});
    } catch (err) {
      setError(
        err instanceof api.ApiError
          ? err.message
          : "Не удалось прочитать server.properties",
      );
    }
  }, [serverId]);

  useEffect(() => {
    void load();
  }, [load]);

  const schemaByKey = useMemo(() => {
    const map = new Map<string, api.Setting>();
    for (const item of settings?.schema ?? []) map.set(item.key, item);
    return map;
  }, [settings]);

  const rows = useMemo(() => {
    if (!settings) return [];

    const keys = Object.keys(settings.values);
    // Known keys first, in schema order, so the settings an operator came for
    // are at the top rather than wherever the file happened to put them.
    const known = settings.schema.map((s) => s.key).filter((key) => key in settings.values);
    const rest = keys.filter((key) => !schemaByKey.has(key)).sort();

    return [...known, ...rest]
      .filter((key) => key.toLowerCase().includes(filter.toLowerCase()))
      .map((key) => ({
        key,
        setting: schemaByKey.get(key),
        value: draft[key] ?? settings.values[key] ?? "",
        managed: key in settings.managed,
        changed: key in draft && draft[key] !== settings.values[key],
      }));
  }, [settings, schemaByKey, draft, filter]);

  const changedCount = useMemo(
    () =>
      settings
        ? Object.keys(draft).filter((key) => draft[key] !== settings.values[key]).length
        : 0,
    [draft, settings],
  );

  function set(key: string, value: string) {
    setNotice(null);
    setDraft((current) => ({ ...current, [key]: value }));
  }

  async function handleSave() {
    if (!settings || changedCount === 0) return;

    const changes: Record<string, string> = {};
    for (const [key, value] of Object.entries(draft)) {
      if (value !== settings.values[key]) changes[key] = value;
    }

    setSaving(true);
    setError(null);
    try {
      const updated = await api.patchSettings(serverId, changes);
      setSettings(updated);
      setDraft({});
      // The restart note only when it is true: a stopped server picks the file
      // up on its next start, and telling an operator to restart something
      // that is not running is the kind of advice that teaches people to
      // ignore notices.
      setNotice(
        updated.restart_required
          ? "Сохранено. Сервер запущен — изменения применятся после перезапуска."
          : "Сохранено.",
      );
    } catch (err) {
      setError(err instanceof api.ApiError ? err.message : "Не удалось сохранить");
    } finally {
      setSaving(false);
    }
  }

  if (!settings) {
    return (
      <section className="card p-4">
        <p className="text-muted">{error ?? "Загрузка…"}</p>
      </section>
    );
  }

  return (
    <section className="card overflow-hidden">
      <header className="flex flex-wrap items-center gap-3 border-b border-line px-4 py-2 text-sm">
        <span className="font-mono text-muted">server.properties</span>
        <input
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          placeholder="Фильтр по ключу"
          className="field h-8 w-48 py-1 text-xs"
        />
        <div className="ml-auto flex items-center gap-3">
          {changedCount > 0 && (
            <span className="text-xs text-warning">Изменений: {changedCount}</span>
          )}
          <button
            type="button"
            className="btn btn-ghost"
            disabled={changedCount === 0 || saving}
            onClick={() => setDraft({})}
          >
            Сбросить
          </button>
          <button
            type="button"
            className="btn btn-primary"
            disabled={changedCount === 0 || saving}
            onClick={() => void handleSave()}
          >
            {saving ? "Сохранение…" : "Сохранить"}
          </button>
        </div>
      </header>

      {error && (
        <p className="border-b border-line bg-danger-bg px-4 py-2 text-sm text-danger">{error}</p>
      )}
      {notice && !error && (
        <p className="border-b border-line px-4 py-2 text-sm text-success">{notice}</p>
      )}

      <div className="divide-y divide-line">
        {rows.length === 0 && (
          <p className="px-4 py-6 text-center text-sm text-muted">Ничего не найдено</p>
        )}

        {rows.map((row) => (
          <div key={row.key} className="grid gap-1 px-4 py-3 sm:grid-cols-[18rem_1fr] sm:gap-4">
            <div>
              <label htmlFor={`setting-${row.key}`} className="font-mono text-sm">
                {row.key}
              </label>
              {row.setting?.note && (
                <p className="mt-0.5 text-xs text-muted">{row.setting.note}</p>
              )}
              {row.managed && (
                <p className="mt-0.5 text-xs text-warning">
                  Этим ключом управляет панель — меняйте его в настройках сервера
                </p>
              )}
              {!row.setting && !row.managed && (
                <p className="mt-0.5 text-xs text-faint">Не из стандартного набора</p>
              )}
            </div>

            <div className="flex items-center gap-2">
              <SettingInput
                id={`setting-${row.key}`}
                setting={row.setting}
                value={row.value}
                disabled={row.managed}
                onChange={(value) => set(row.key, value)}
              />
              {row.changed && <span className="text-xs text-warning">•</span>}
              {row.setting?.default !== undefined && row.value !== row.setting.default && (
                <button
                  type="button"
                  className="text-xs text-accent hover:underline"
                  disabled={row.managed}
                  onClick={() => set(row.key, row.setting?.default ?? "")}
                >
                  по умолчанию
                </button>
              )}
            </div>
          </div>
        ))}
      </div>
    </section>
  );
}

function SettingInput({
  id,
  setting,
  value,
  disabled,
  onChange,
}: {
  id: string;
  setting: api.Setting | undefined;
  value: string;
  disabled: boolean;
  onChange: (value: string) => void;
}) {
  // A key the schema does not know is edited as text: guessing a type for it
  // would risk rewriting a mod's setting into something it cannot read.
  if (!setting || setting.kind === "string") {
    return (
      <input
        id={id}
        value={value}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value)}
        className="field h-9 font-mono text-sm"
      />
    );
  }

  if (setting.kind === "bool") {
    return (
      <label className="flex items-center gap-2 text-sm">
        <input
          id={id}
          type="checkbox"
          checked={value === "true"}
          disabled={disabled}
          onChange={(e) => onChange(e.target.checked ? "true" : "false")}
        />
        <span className="font-mono text-muted">{value}</span>
      </label>
    );
  }

  if (setting.kind === "enum") {
    return (
      <select
        id={id}
        value={value}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value)}
        className="field h-9 font-mono text-sm"
      >
        {/* A value the server already has but the list does not is kept, not
            silently replaced by the first option. */}
        {!(setting.values ?? []).includes(value) && <option value={value}>{value}</option>}
        {(setting.values ?? []).map((option) => (
          <option key={option} value={option}>
            {option}
          </option>
        ))}
      </select>
    );
  }

  return (
    <input
      id={id}
      type="number"
      value={value}
      min={setting.min}
      max={setting.max}
      disabled={disabled}
      onChange={(e) => onChange(e.target.value)}
      className="field h-9 w-40 font-mono text-sm"
    />
  );
}
