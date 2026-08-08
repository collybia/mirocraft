import { useCallback, useEffect, useState, type FormEvent } from "react";

import * as api from "../lib/api";

interface Props {
  /** The signed-in administrator, so the page can refuse to lock them out. */
  me: api.Me;
}

export function Admin({ me }: Props) {
  const [users, setUsers] = useState<api.User[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  const [form, setForm] = useState({
    email: "",
    password: "",
    role: "user" as "user" | "admin",
    max_servers: 0,
    max_ram_mb: 0,
  });

  const reload = useCallback(async () => {
    setLoading(true);
    try {
      setUsers(await api.listUsers());
      setError(null);
    } catch (err) {
      setError(err instanceof api.ApiError ? err.message : "Не удалось прочитать список");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void reload();
  }, [reload]);

  function report(err: unknown, fallback: string) {
    setError(err instanceof api.ApiError ? err.message : fallback);
  }

  async function handleCreate(event: FormEvent) {
    event.preventDefault();
    setBusy("new");
    setError(null);
    setNotice(null);
    try {
      await api.createUser({
        email: form.email.trim(),
        password: form.password,
        role: form.role,
        max_servers: form.max_servers,
        max_ram_mb: form.max_ram_mb,
      });
      setNotice(`Учётная запись ${form.email.trim()} создана`);
      setForm({ email: "", password: "", role: "user", max_servers: 0, max_ram_mb: 0 });
      await reload();
    } catch (err) {
      report(err, "Не удалось создать учётную запись");
    } finally {
      setBusy(null);
    }
  }

  async function patch(user: api.User, patchBody: Parameters<typeof api.patchUser>[1]) {
    setBusy(user.id);
    setError(null);
    try {
      await api.patchUser(user.id, patchBody);
      await reload();
    } catch (err) {
      report(err, "Не удалось изменить учётную запись");
    } finally {
      setBusy(null);
    }
  }

  async function handleDelete(user: api.User) {
    if (
      !window.confirm(
        `Удалить ${user.email}? Серверы этого пользователя останутся, но управлять ими будет некому.`,
      )
    ) {
      return;
    }

    setBusy(user.id);
    setError(null);
    try {
      await api.deleteUser(user.id);
      await reload();
    } catch (err) {
      report(err, "Не удалось удалить");
    } finally {
      setBusy(null);
    }
  }

  async function handleResetPassword(user: api.User) {
    const password = window.prompt(`Новый пароль для ${user.email}:`);
    if (!password) return;
    await patch(user, { password });
    setNotice(`Пароль для ${user.email} изменён`);
  }

  return (
    <div className="grid gap-4">
      <h1 className="text-lg font-semibold">Пользователи</h1>

      {error && (
        <p className="rounded-sm border border-danger bg-danger-bg px-3 py-2 text-sm text-danger">
          {error}
        </p>
      )}
      {notice && !error && <p className="text-sm text-success">{notice}</p>}

      <section className="card overflow-hidden">
        <table className="w-full text-sm">
          <thead className="text-muted">
            <tr className="border-b border-line">
              <th className="px-4 py-2 text-left font-normal">Логин</th>
              <th className="px-4 py-2 text-left font-normal">Роль</th>
              <th className="px-4 py-2 text-left font-normal">Лимиты</th>
              <th className="px-4 py-2 text-left font-normal">Создан</th>
              <th className="px-4 py-2" />
            </tr>
          </thead>
          <tbody>
            {loading && (
              <tr>
                <td colSpan={5} className="px-4 py-6 text-center text-muted">
                  Загрузка…
                </td>
              </tr>
            )}

            {!loading &&
              users.map((user) => {
                const isSelf = user.id === me.id;
                return (
                  <tr key={user.id} className="border-b border-line last:border-0">
                    <td className="px-4 py-2">
                      {user.email}
                      {isSelf && <span className="ml-2 text-xs text-faint">это вы</span>}
                      {user.blocked && (
                        <span className="ml-2 text-xs text-danger">заблокирован</span>
                      )}
                    </td>
                    <td className="px-4 py-2 text-muted">
                      {user.role === "admin" ? "администратор" : "пользователь"}
                    </td>
                    <td className="px-4 py-2 text-muted">{formatLimits(user)}</td>
                    <td className="px-4 py-2 text-muted">{formatDate(user.created_at)}</td>
                    <td className="px-4 py-2">
                      <div className="flex justify-end gap-3">
                        <button
                          type="button"
                          className="text-xs text-accent hover:underline"
                          disabled={busy !== null}
                          onClick={() => void handleResetPassword(user)}
                        >
                          Сменить пароль
                        </button>

                        {/* Blocking or demoting yourself locks you out of the
                            panel you are standing in, so the buttons are simply
                            not offered rather than failing on the server. */}
                        {!isSelf && (
                          <>
                            <button
                              type="button"
                              className="text-xs text-accent hover:underline"
                              disabled={busy !== null}
                              onClick={() =>
                                void patch(user, {
                                  role: user.role === "admin" ? "user" : "admin",
                                })
                              }
                            >
                              {user.role === "admin" ? "Снять админа" : "Сделать админом"}
                            </button>
                            <button
                              type="button"
                              className="text-xs text-warning hover:underline"
                              disabled={busy !== null}
                              onClick={() => void patch(user, { blocked: !user.blocked })}
                            >
                              {user.blocked ? "Разблокировать" : "Заблокировать"}
                            </button>
                            <button
                              type="button"
                              className="text-xs text-danger hover:underline"
                              disabled={busy !== null}
                              onClick={() => void handleDelete(user)}
                            >
                              Удалить
                            </button>
                          </>
                        )}
                      </div>
                    </td>
                  </tr>
                );
              })}
          </tbody>
        </table>
      </section>

      <section className="card p-4">
        <h2 className="mb-3 text-sm text-muted">Новая учётная запись</h2>

        <form onSubmit={handleCreate} className="grid gap-3">
          <div className="flex flex-wrap items-end gap-3">
            <div>
              <label htmlFor="new-email" className="block text-xs text-muted">
                Логин
              </label>
              <input
                id="new-email"
                value={form.email}
                onChange={(e) => setForm({ ...form, email: e.target.value })}
                required
                className="field h-9 w-56 text-sm"
              />
            </div>

            <div>
              <label htmlFor="new-password" className="block text-xs text-muted">
                Пароль
              </label>
              <input
                id="new-password"
                type="password"
                value={form.password}
                onChange={(e) => setForm({ ...form, password: e.target.value })}
                required
                className="field h-9 w-56 text-sm"
              />
            </div>

            <div>
              <label htmlFor="new-role" className="block text-xs text-muted">
                Роль
              </label>
              <select
                id="new-role"
                value={form.role}
                onChange={(e) => setForm({ ...form, role: e.target.value as "user" | "admin" })}
                className="field h-9 w-40 text-sm"
              >
                <option value="user">пользователь</option>
                <option value="admin">администратор</option>
              </select>
            </div>

            <div>
              <label htmlFor="new-servers" className="block text-xs text-muted">
                Серверов, 0 — без лимита
              </label>
              <input
                id="new-servers"
                type="number"
                min={0}
                value={form.max_servers}
                onChange={(e) => setForm({ ...form, max_servers: Number(e.target.value) })}
                className="field h-9 w-32 text-sm"
              />
            </div>

            <div>
              <label htmlFor="new-ram" className="block text-xs text-muted">
                Память, МБ
              </label>
              <input
                id="new-ram"
                type="number"
                min={0}
                step={512}
                value={form.max_ram_mb}
                onChange={(e) => setForm({ ...form, max_ram_mb: Number(e.target.value) })}
                className="field h-9 w-32 text-sm"
              />
            </div>

            <button type="submit" className="btn btn-primary" disabled={busy === "new"}>
              Создать
            </button>
          </div>
        </form>
      </section>
    </div>
  );
}

function formatLimits(user: api.User): string {
  const parts: string[] = [];
  if (user.max_servers > 0) parts.push(`${user.max_servers} серв.`);
  if (user.max_ram_mb > 0) parts.push(`${user.max_ram_mb} МБ`);
  return parts.length > 0 ? parts.join(" · ") : "без лимитов";
}

function formatDate(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "—";
  return date.toLocaleDateString("ru-RU");
}
