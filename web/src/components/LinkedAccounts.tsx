import { useCallback, useEffect, useState } from "react";

import * as api from "../lib/api";

/*
 * Where a person links their Discord or Telegram to this account.
 *
 * The code is shown once and lives ten minutes, so the section shows the
 * remaining time rather than a static string: a code that quietly expired
 * while someone was switching windows is the commonest way this flow fails.
 */

const LABELS: Record<api.BotProvider, string> = {
  discord: "Discord",
  telegram: "Telegram",
};

export function LinkedAccounts() {
  const [links, setLinks] = useState<api.Integration[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<api.BotProvider | null>(null);
  const [issued, setIssued] = useState<{
    provider: api.BotProvider;
    code: string;
    expiresAt: number;
  } | null>(null);
  const [remaining, setRemaining] = useState(0);

  const reload = useCallback(async () => {
    try {
      setLinks(await api.listIntegrations());
      setError(null);
    } catch (err) {
      setError(err instanceof api.ApiError ? err.message : "Не удалось прочитать привязки");
    }
  }, []);

  useEffect(() => {
    void reload();
  }, [reload]);

  // Counts the issued code down and forgets it when it expires, so nobody
  // types a code the panel has already stopped accepting.
  useEffect(() => {
    if (!issued) return;

    const tick = () => {
      const left = Math.max(0, Math.round((issued.expiresAt - Date.now()) / 1000));
      setRemaining(left);
      if (left === 0) setIssued(null);
    };
    tick();

    const timer = setInterval(tick, 1000);
    return () => clearInterval(timer);
  }, [issued]);

  async function requestCode(provider: api.BotProvider) {
    setBusy(provider);
    setError(null);
    try {
      const result = await api.createLinkCode(provider);
      setIssued({
        provider,
        code: result.code,
        expiresAt: new Date(result.expires_at).getTime(),
      });
    } catch (err) {
      setError(err instanceof api.ApiError ? err.message : "Не удалось получить код");
    } finally {
      setBusy(null);
    }
  }

  async function unlink(provider: api.BotProvider) {
    setBusy(provider);
    setError(null);
    try {
      await api.unlinkIntegration(provider);
      await reload();
    } catch (err) {
      setError(err instanceof api.ApiError ? err.message : "Не удалось отвязать");
    } finally {
      setBusy(null);
    }
  }

  const linked = new Map(links.map((link) => [link.provider, link]));

  return (
    <section className="card p-5">
      <h2 className="mb-1 font-semibold">Discord и Telegram</h2>
      <p className="mb-4 text-sm text-muted">
        Привяжите чат-аккаунт, чтобы управлять своими серверами командами бота. Бот получит ровно
        ваши права и только на ваши серверы.
      </p>

      {error && (
        <p className="mb-3 rounded-sm border border-danger bg-danger-bg px-3 py-2 text-sm text-danger">
          {error}
        </p>
      )}

      <div className="grid gap-3">
        {(Object.keys(LABELS) as api.BotProvider[]).map((provider) => {
          const link = linked.get(provider);
          const showing = issued?.provider === provider ? issued : null;

          return (
            <div key={provider} className="grid gap-2 border-b border-line pb-3 last:border-0 last:pb-0">
              <div className="flex flex-wrap items-center justify-between gap-3">
                <div>
                  <span className="text-sm">{LABELS[provider]}</span>
                  <span className="ml-2 text-xs text-muted">
                    {link ? `привязан: ${link.external_id}` : "не привязан"}
                  </span>
                </div>

                {link ? (
                  <button
                    type="button"
                    className="text-xs text-accent hover:underline"
                    disabled={busy === provider}
                    onClick={() => void unlink(provider)}
                  >
                    отвязать
                  </button>
                ) : (
                  <button
                    type="button"
                    className="btn"
                    disabled={busy === provider}
                    onClick={() => void requestCode(provider)}
                  >
                    Получить код
                  </button>
                )}
              </div>

              {showing && (
                <div className="rounded-sm border border-line bg-elevated px-3 py-2">
                  <p className="text-sm">
                    Отправьте боту{" "}
                    <code className="font-mono">/link {showing.code}</code>
                  </p>
                  <p className="mt-1 text-xs text-muted">
                    Код сработает один раз, осталось {formatRemaining(remaining)}.
                  </p>
                </div>
              )}
            </div>
          );
        })}
      </div>
    </section>
  );
}

function formatRemaining(seconds: number): string {
  const minutes = Math.floor(seconds / 60);
  const rest = seconds % 60;
  if (minutes === 0) return `${rest} с`;
  return `${minutes}:${String(rest).padStart(2, "0")}`;
}
