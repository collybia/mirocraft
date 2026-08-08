import { useEffect, useRef, useState } from "react";

import * as api from "../lib/api";
import { customChoice, customId, isCustom } from "../lib/theme";
import {
  BUILTIN_THEMES,
  EDITOR_DEFAULTS,
  SYSTEM,
  accentForeground,
  swatchFor,
} from "../themes";
import { useTheme } from "../ThemeProvider";

export function Settings({ user }: { user: api.Me }) {
  return (
    <div className="grid gap-6">
      <ThemeSection />
      <ThemeEditor />
      <AccountSection user={user} />
    </div>
  );
}

/* --- built-in theme picker --- */

function ThemeSection() {
  const { choice, customThemes, setChoice } = useTheme();

  return (
    <section className="card p-5">
      <h2 className="mb-1 font-semibold">Тема оформления</h2>
      <p className="mb-4 text-sm text-muted">
        Выбор хранится в профиле, поэтому переезжает вместе с вами в другой браузер.
      </p>

      <div className="grid gap-3 sm:grid-cols-3">
        <ThemeCard
          id={SYSTEM}
          name="Как в системе"
          selected={choice === SYSTEM}
          onSelect={() => void setChoice(SYSTEM)}
          swatch={{ bg: "var(--bg-elevated)", text: "var(--text)", accent: "var(--accent)" }}
        />

        {BUILTIN_THEMES.map((theme) => (
          <ThemeCard
            key={theme.id}
            id={theme.id}
            name={theme.name}
            selected={choice === theme.id}
            onSelect={() => void setChoice(theme.id)}
            swatch={theme.preview}
          />
        ))}

        {customThemes.map((theme) => (
          <ThemeCard
            key={theme.id}
            id={theme.id}
            name={theme.name}
            selected={choice === customChoice(theme.id)}
            onSelect={() => void setChoice(customChoice(theme.id))}
            swatch={swatchFor(theme.base, theme.vars)}
            custom
          />
        ))}
      </div>
    </section>
  );
}

interface ThemeCardProps {
  id: string;
  name: string;
  selected: boolean;
  onSelect: () => void;
  swatch: { bg: string; text: string; accent: string };
  custom?: boolean;
}

function ThemeCard({ name, selected, onSelect, swatch, custom }: ThemeCardProps) {
  return (
    <button
      type="button"
      onClick={onSelect}
      aria-pressed={selected}
      className={[
        "rounded border p-3 text-left",
        selected ? "border-accent" : "border-line hover:border-line-strong",
      ].join(" ")}
    >
      <div
        className="mb-2 flex h-12 items-center gap-2 rounded-sm px-2"
        style={{ backgroundColor: swatch.bg }}
      >
        <span className="h-4 w-4 rounded-full" style={{ backgroundColor: swatch.accent }} />
        <span className="text-xs" style={{ color: swatch.text }}>
          Aa
        </span>
      </div>
      <div className="text-sm text-body">
        {name}
        {custom && <span className="ml-1 text-xs text-faint">своя</span>}
      </div>
    </button>
  );
}

/* --- custom theme editor --- */

const DEFAULT_ACCENT = EDITOR_DEFAULTS.accent;
const DEFAULT_RADIUS = EDITOR_DEFAULTS.radiusPx;

function ThemeEditor() {
  const { customThemes, reloadCustomThemes, setChoice, preview, endPreview, choice } = useTheme();

  const [name, setName] = useState("Моя тема");
  const [base, setBase] = useState<"dark" | "light">("dark");
  const [accent, setAccent] = useState<string>(DEFAULT_ACCENT);
  const [radius, setRadius] = useState<number>(DEFAULT_RADIUS);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const fileRef = useRef<HTMLInputElement | null>(null);

  const vars = {
    "--accent": accent,
    "--accent-hover": accent,
    "--accent-fg": accentForeground(base),
    "--radius": `${radius}px`,
    "--radius-sm": `${Math.max(2, Math.round(radius * 0.6))}px`,
    "--radius-lg": `${Math.round(radius * 1.6)}px`,
  };

  // The preview paints the page itself, so what you see is literally the
  // theme applied — not a mock-up that can drift from the real thing.
  useEffect(() => {
    preview(vars, base);
    return () => endPreview();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [accent, radius, base]);

  async function handleSave() {
    setError(null);
    setBusy(true);
    try {
      const payload = { schema: "mirocraft.theme/v1", name, base, vars };
      const saved = editingId
        ? await api.updateCustomTheme(editingId, payload)
        : await api.createCustomTheme(payload);

      await reloadCustomThemes();
      await setChoice(customChoice(saved.id));
      setEditingId(saved.id);
    } catch (err) {
      setError(err instanceof api.ApiError ? err.message : "Не удалось сохранить тему");
    } finally {
      setBusy(false);
    }
  }

  async function handleDelete(id: string) {
    try {
      await api.deleteCustomTheme(id);
      if (isCustom(choice) && customId(choice) === id) {
        await setChoice(base);
      }
      if (editingId === id) setEditingId(null);
      await reloadCustomThemes();
    } catch (err) {
      setError(err instanceof api.ApiError ? err.message : "Не удалось удалить тему");
    }
  }

  function handleExport() {
    const doc = { schema: "mirocraft.theme/v1", name, base, vars };
    const blob = new Blob([JSON.stringify(doc, null, 2)], { type: "application/json" });
    const url = URL.createObjectURL(blob);

    const link = document.createElement("a");
    link.href = url;
    link.download = `${name.replace(/[^\w-]+/g, "-").toLowerCase()}.mirocraft-theme.json`;
    link.click();
    URL.revokeObjectURL(url);
  }

  async function handleImport(file: File) {
    setError(null);
    try {
      const doc = JSON.parse(await file.text());
      // The server validates properly; this only fills the form, so a bad file
      // shows a message rather than silently doing nothing.
      if (doc.schema && doc.schema !== "mirocraft.theme/v1") {
        setError("Неподдерживаемая версия формата темы");
        return;
      }
      setName(doc.name ?? "Импортированная тема");
      setBase(doc.base === "light" ? "light" : "dark");
      setAccent(doc.vars?.["--accent"] ?? DEFAULT_ACCENT);
      setRadius(parseInt(doc.vars?.["--radius"] ?? `${DEFAULT_RADIUS}`, 10) || DEFAULT_RADIUS);
      setEditingId(null);
    } catch {
      setError("Файл не похож на тему Mirocraft");
    }
  }

  return (
    <section className="card p-5">
      <h2 className="mb-1 font-semibold">Своя тема</h2>
      <p className="mb-4 text-sm text-muted">
        Возьмите светлую или тёмную основу, поменяйте акцент и скругления. Превью применяется
        сразу — прямо к этой странице.
      </p>

      <div className="grid gap-4 sm:grid-cols-2">
        <div>
          <label className="mb-1 block text-sm text-muted" htmlFor="theme-name">
            Название
          </label>
          <input
            id="theme-name"
            value={name}
            onChange={(e) => setName(e.target.value)}
            maxLength={64}
            className="field"
          />
        </div>

        <div>
          <label className="mb-1 block text-sm text-muted" htmlFor="theme-base">
            Основа
          </label>
          <select
            id="theme-base"
            value={base}
            onChange={(e) => setBase(e.target.value as "dark" | "light")}
            className="field"
          >
            <option value="dark">Тёмная</option>
            <option value="light">Светлая</option>
          </select>
        </div>

        <div>
          <label className="mb-1 block text-sm text-muted" htmlFor="theme-accent">
            Акцентный цвет
          </label>
          <div className="flex items-center gap-2">
            <input
              id="theme-accent"
              type="color"
              value={accent}
              onChange={(e) => setAccent(e.target.value)}
              className="h-9 w-14 rounded-sm border border-line-strong bg-inset"
            />
            <code className="text-sm text-muted">{accent}</code>
          </div>
        </div>

        <div>
          <label className="mb-1 block text-sm text-muted" htmlFor="theme-radius">
            Скругление: {radius}px
          </label>
          <input
            id="theme-radius"
            type="range"
            min={0}
            max={24}
            value={radius}
            onChange={(e) => setRadius(Number(e.target.value))}
            className="w-full"
          />
        </div>
      </div>

      {error && (
        <p className="mt-4 rounded-sm border border-danger bg-danger-bg px-3 py-2 text-sm text-danger">
          {error}
        </p>
      )}

      <div className="mt-4 flex flex-wrap gap-2">
        <button type="button" onClick={() => void handleSave()} disabled={busy} className="btn btn-primary">
          {editingId ? "Сохранить" : "Создать тему"}
        </button>
        <button type="button" onClick={handleExport} className="btn btn-ghost">
          Экспорт JSON
        </button>
        <button type="button" onClick={() => fileRef.current?.click()} className="btn btn-ghost">
          Импорт JSON
        </button>
        <input
          ref={fileRef}
          type="file"
          accept="application/json,.json"
          hidden
          onChange={(e) => {
            const file = e.target.files?.[0];
            if (file) void handleImport(file);
            e.target.value = "";
          }}
        />
      </div>

      {customThemes.length > 0 && (
        <ul className="mt-5 grid gap-2">
          {customThemes.map((theme) => (
            <li
              key={theme.id}
              className="flex items-center gap-3 rounded-sm border border-line px-3 py-2"
            >
              <span
                className="h-4 w-4 rounded-full"
                style={{ backgroundColor: theme.vars["--accent"] ?? "var(--accent)" }}
              />
              <span className="text-sm text-body">{theme.name}</span>
              <span className="text-xs text-faint">{theme.base}</span>

              <div className="ml-auto flex gap-2">
                <button
                  type="button"
                  className="btn btn-ghost px-2 py-1 text-xs"
                  onClick={() => {
                    setEditingId(theme.id);
                    setName(theme.name);
                    setBase(theme.base);
                    setAccent(theme.vars["--accent"] ?? DEFAULT_ACCENT);
                    setRadius(parseInt(theme.vars["--radius"] ?? "10", 10) || DEFAULT_RADIUS);
                  }}
                >
                  Править
                </button>
                <button
                  type="button"
                  className="btn btn-ghost px-2 py-1 text-xs"
                  onClick={() => void handleDelete(theme.id)}
                >
                  Удалить
                </button>
              </div>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

/* --- account --- */

function AccountSection({ user }: { user: api.Me }) {
  const [oldPassword, setOldPassword] = useState("");
  const [password, setPassword] = useState("");
  const [message, setMessage] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  async function handleSubmit() {
    setError(null);
    setMessage(null);
    try {
      await api.updateMe({ password, old_password: oldPassword });
      setMessage("Пароль изменён");
      setOldPassword("");
      setPassword("");
    } catch (err) {
      setError(err instanceof api.ApiError ? err.message : "Не удалось изменить пароль");
    }
  }

  return (
    <section className="card p-5">
      <h2 className="mb-1 font-semibold">Аккаунт</h2>
      <p className="mb-4 text-sm text-muted">
        {user.email} · роль: {user.role === "admin" ? "администратор" : "пользователь"}
      </p>

      <div className="grid gap-3 sm:grid-cols-2">
        <div>
          <label className="mb-1 block text-sm text-muted" htmlFor="old-password">
            Текущий пароль
          </label>
          <input
            id="old-password"
            type="password"
            autoComplete="current-password"
            value={oldPassword}
            onChange={(e) => setOldPassword(e.target.value)}
            className="field"
          />
        </div>
        <div>
          <label className="mb-1 block text-sm text-muted" htmlFor="new-password">
            Новый пароль
          </label>
          <input
            id="new-password"
            type="password"
            autoComplete="new-password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            className="field"
          />
        </div>
      </div>

      {message && <p className="mt-3 text-sm text-success">{message}</p>}
      {error && <p className="mt-3 text-sm text-danger">{error}</p>}

      <button
        type="button"
        onClick={() => void handleSubmit()}
        disabled={!oldPassword || !password}
        className="btn btn-primary mt-4"
      >
        Сменить пароль
      </button>
    </section>
  );
}
