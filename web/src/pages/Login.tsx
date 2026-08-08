import { useState, type FormEvent } from "react";

import * as api from "../lib/api";

interface Props {
  onLoggedIn: () => void;
}

export function Login({ onLoggedIn }: Props) {
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function handleSubmit(event: FormEvent) {
    event.preventDefault();
    setError(null);
    setBusy(true);

    try {
      await api.login(email, password);
      onLoggedIn();
    } catch (err) {
      // The server deliberately does not say which half was wrong, and neither
      // does the panel: repeating a specific reason would undo that.
      if (err instanceof api.ApiError && err.code === "rate_limited") {
        setError("Слишком много попыток. Подождите минуту.");
      } else {
        setError("Неверный логин или пароль.");
      }
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-surface px-4">
      <form onSubmit={handleSubmit} className="card w-full max-w-sm p-6">
        <h1 className="mb-1 text-xl font-semibold text-body">Mirocraft</h1>
        <p className="mb-6 text-sm text-muted">Панель управления серверами</p>

        <label className="mb-1 block text-sm text-muted" htmlFor="email">
          Логин
        </label>
        {/* Deliberately not type="email": the panel accepts a plain login too,
            and the browser's own validation would reject one before the
            request is ever sent. */}
        <input
          id="email"
          type="text"
          autoComplete="username"
          autoCapitalize="none"
          autoCorrect="off"
          spellCheck={false}
          required
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          placeholder="admin"
          className="field mb-4"
        />

        <label className="mb-1 block text-sm text-muted" htmlFor="password">
          Пароль
        </label>
        <input
          id="password"
          type="password"
          autoComplete="current-password"
          required
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          className="field mb-4"
        />

        {error && (
          <p className="mb-4 rounded-sm border border-danger bg-danger-bg px-3 py-2 text-sm text-danger">
            {error}
          </p>
        )}

        <button type="submit" disabled={busy} className="btn btn-primary w-full">
          {busy ? "Вход…" : "Войти"}
        </button>
      </form>
    </div>
  );
}
