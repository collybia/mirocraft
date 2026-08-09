import { useCallback, useEffect, useState } from "react";

import * as api from "../lib/api";

/*
 * The page where an operator turns a chat bot on.
 *
 * The token is write-only: it goes up and never comes back, so the field
 * always starts empty and shows the stored token's last characters beside it.
 * A field pre-filled with dots that submits them unchanged is how a token gets
 * overwritten with dots.
 */

const LABELS: Record<api.BotProvider, { name: string; help: string; where: string }> = {
  discord: {
    name: "Discord",
    help: "Создайте приложение, добавьте бота и скопируйте его токен. Приглашая бота на сервер, дайте право applications.commands.",
    where: "https://discord.com/developers/applications",
  },
  telegram: {
    name: "Telegram",
    help: "Напишите @BotFather команду /newbot и скопируйте выданный токен.",
    where: "https://t.me/BotFather",
  },
};

export function BotSettings() {
  const [bots, setBots] = useState<api.BotSettings[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState<api.BotProvider | null>(null);
  const [tokens, setTokens] = useState<Partial<Record<api.BotProvider, string>>>({});

  const reload = useCallback(async () => {
    try {
      setBots(await api.listBots());
      setError(null);
    } catch (err) {
      setError(err instanceof api.ApiError ? err.message : "Не удалось прочитать настройки ботов");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void reload();
  }, [reload]);

  // A bot that is connecting or has just been switched on takes a few seconds
  // to reach the platform, so the page follows it rather than leaving the
  // operator to guess whether to refresh.
  useEffect(() => {
    const pending = bots.some((bot) => bot.enabled && !bot.running && bot.status !== "failed");
    if (!pending) return;

    const timer = setTimeout(() => void reload(), 2000);
    return () => clearTimeout(timer);
  }, [bots, reload]);

  async function save(provider: api.BotProvider, patch: { token?: string; enabled?: boolean }) {
    setBusy(provider);
    setError(null);
    setNotice(null);
    try {
      await api.saveBot(provider, patch);
      setTokens((current) => ({ ...current, [provider]: "" }));
      setNotice(`${LABELS[provider].name}: настройки сохранены`);
      await reload();
    } catch (err) {
      setError(err instanceof api.ApiError ? err.message : "Не удалось сохранить");
    } finally {
      setBusy(null);
    }
  }

  async function forget(provider: api.BotProvider) {
    if (!confirm(`Забыть токен ${LABELS[provider].name}? Бот остановится.`)) return;

    setBusy(provider);
    setError(null);
    try {
      await api.deleteBot(provider);
      setNotice(`${LABELS[provider].name}: токен удалён`);
      await reload();
    } catch (err) {
      setError(err instanceof api.ApiError ? err.message : "Не удалось удалить");
    } finally {
      setBusy(null);
    }
  }

  return (
    <div className="grid gap-4">
      <div>
        <h1 className="text-lg font-semibold">Боты</h1>
        <p className="mt-1 text-sm text-muted">
          Бот показывает игрокам их серверы и умеет их запускать. Права он берёт у того, кто
          привязал свой аккаунт: чужие серверы через бота не видны.
        </p>
      </div>

      {error && (
        <p className="rounded-sm border border-danger bg-danger-bg px-3 py-2 text-sm text-danger">
          {error}
        </p>
      )}
      {notice && !error && <p className="text-sm text-success">{notice}</p>}

      {loading && <p className="text-sm text-muted">Загружаю…</p>}

      {bots.map((bot) => {
        const label = LABELS[bot.provider];
        const pending = busy === bot.provider;

        return (
          <section key={bot.provider} className="card grid gap-3 p-4">
            <div className="flex flex-wrap items-center justify-between gap-3">
              <div>
                <h2 className="font-medium">{label.name}</h2>
                <p className="text-xs text-muted">
                  <StatusLine bot={bot} />
                </p>
              </div>

              <label className="flex items-center gap-2 text-sm">
                <input
                  type="checkbox"
                  checked={bot.enabled}
                  disabled={pending || !bot.configured}
                  onChange={(event) => void save(bot.provider, { enabled: event.target.checked })}
                />
                Включён
              </label>
            </div>

            {bot.error && (
              <p className="rounded-sm border border-warning bg-warning-bg px-3 py-2 text-sm text-warning">
                {bot.error}
              </p>
            )}

            <div className="flex flex-wrap items-end gap-3">
              <label className="grid gap-1">
                <span className="block text-xs text-muted">
                  Токен бота
                  {bot.configured && (
                    <span className="ml-2 text-faint">сохранён, оканчивается на {bot.token_hint}</span>
                  )}
                </span>
                <input
                  type="password"
                  autoComplete="off"
                  className="field h-9 w-80 text-sm"
                  placeholder={bot.configured ? "оставьте пустым, чтобы не менять" : "вставьте токен"}
                  value={tokens[bot.provider] ?? ""}
                  onChange={(event) =>
                    setTokens((current) => ({ ...current, [bot.provider]: event.target.value }))
                  }
                />
              </label>

              <button
                type="button"
                className="btn btn-primary"
                disabled={pending || !(tokens[bot.provider] ?? "").trim()}
                onClick={() =>
                  void save(bot.provider, {
                    token: (tokens[bot.provider] ?? "").trim(),
                    // Pasting a token is the moment someone means to switch
                    // the bot on; asking for a second click would be asking
                    // twice for one decision.
                    enabled: true,
                  })
                }
              >
                Сохранить и включить
              </button>

              {bot.configured && (
                <button
                  type="button"
                  className="text-xs text-accent hover:underline"
                  disabled={pending}
                  onClick={() => void forget(bot.provider)}
                >
                  забыть токен
                </button>
              )}
            </div>

            <p className="text-xs text-muted">
              {label.help}{" "}
              <a className="text-accent hover:underline" href={label.where} target="_blank" rel="noreferrer">
                {label.where}
              </a>
            </p>
          </section>
        );
      })}
    </div>
  );
}

function StatusLine({ bot }: { bot: api.BotSettings }) {
  if (!bot.configured) return <>токен не сохранён</>;
  if (!bot.enabled) return <>выключен</>;
  if (bot.running) {
    return <>подключён{bot.account ? ` как ${bot.account}` : ""}</>;
  }
  if (bot.status === "failed") return <>не подключился</>;
  return <>подключается…</>;
}
