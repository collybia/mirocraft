import { useCallback, useEffect, useState } from "react";

import * as api from "../lib/api";

interface Props {
  serverId: string;
  /** Restoring overwrites the world, so the tab needs to know the state. */
  running: boolean;
}

/** Cron presets, so the common cases need no cron knowledge at all. */
const PRESETS: { label: string; cron: string }[] = [
  { label: "Каждый день в 4:00", cron: "0 4 * * *" },
  { label: "Каждые 6 часов", cron: "0 */6 * * *" },
  { label: "Каждый понедельник в 5:00", cron: "0 5 * * 1" },
];

export function ServerBackups({ serverId, running }: Props) {
  const [backups, setBackups] = useState<api.Backup[]>([]);
  const [schedule, setSchedule] = useState<api.BackupSchedule | null>(null);
  const [draft, setDraft] = useState<{ cron: string; keep_last: number; enabled: boolean } | null>(
    null,
  );
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const reload = useCallback(async () => {
    try {
      setBackups(await api.listBackups(serverId));
    } catch (err) {
      setError(err instanceof api.ApiError ? err.message : "Не удалось прочитать бэкапы");
    }
  }, [serverId]);

  useEffect(() => {
    void reload();

    void (async () => {
      try {
        const loaded = await api.getBackupSchedule(serverId);
        setSchedule(loaded);
        setDraft({ cron: loaded.cron, keep_last: loaded.keep_last, enabled: loaded.enabled });
      } catch {
        // A server with no schedule yet is the ordinary case, not an error.
        setDraft({ cron: "0 4 * * *", keep_last: 7, enabled: false });
      }
    })();
  }, [serverId, reload]);

  // A backup in flight becomes done a few seconds later, and polling only
  // while something is pending keeps an idle tab quiet.
  const pending = backups.some((b) => b.state === "pending" || b.state === "running");
  useEffect(() => {
    if (!pending) return;
    const timer = setInterval(() => void reload(), 3000);
    return () => clearInterval(timer);
  }, [pending, reload]);

  function report(err: unknown, fallback: string) {
    setError(err instanceof api.ApiError ? err.message : fallback);
  }

  async function handleCreate() {
    const note = window.prompt("Пометка к бэкапу (необязательно):", "");
    if (note === null) return;

    setBusy(true);
    setError(null);
    try {
      await api.createBackup(serverId, note);
      setNotice(
        running
          ? "Бэкап начат. Сервер попросят сбросить мир на диск, чтобы архив не поймал его на середине записи."
          : "Бэкап начат.",
      );
      await reload();
    } catch (err) {
      report(err, "Не удалось начать бэкап");
    } finally {
      setBusy(false);
    }
  }

  async function handleRestore(backup: api.Backup) {
    const typed = window.prompt(
      "Восстановление заменит текущие файлы сервера содержимым архива.\n" +
        "Текущий мир будет потерян. Введите RESTORE для подтверждения:",
    );
    if (typed !== "RESTORE") return;

    setBusy(true);
    setError(null);
    try {
      await api.restoreBackup(serverId, backup.id);
      setNotice("Восстановление запущено.");
    } catch (err) {
      report(err, "Не удалось восстановить");
    } finally {
      setBusy(false);
    }
  }

  async function handleDelete(backup: api.Backup) {
    if (!window.confirm(`Удалить бэкап от ${formatDate(backup.created_at)}?`)) return;

    setBusy(true);
    setError(null);
    try {
      await api.deleteBackup(serverId, backup.id);
      await reload();
    } catch (err) {
      report(err, "Не удалось удалить");
    } finally {
      setBusy(false);
    }
  }

  async function handleDownload(backup: api.Backup) {
    setError(null);
    try {
      await api.downloadBackup(serverId, backup.id, `${serverId}-${backup.id}.zip`);
    } catch (err) {
      report(err, "Не удалось скачать");
    }
  }

  async function handleSaveSchedule() {
    if (!draft) return;

    setBusy(true);
    setError(null);
    try {
      const saved = await api.putBackupSchedule(serverId, draft);
      setSchedule(saved);
      setNotice(
        saved.enabled && saved.next_run_at
          ? `Расписание сохранено. Следующий запуск: ${formatDate(saved.next_run_at)}`
          : "Расписание сохранено.",
      );
    } catch (err) {
      report(err, "Не удалось сохранить расписание");
    } finally {
      setBusy(false);
    }
  }

  const scheduleChanged =
    draft !== null &&
    (schedule === null ||
      draft.cron !== schedule.cron ||
      draft.keep_last !== schedule.keep_last ||
      draft.enabled !== schedule.enabled);

  return (
    <div className="grid gap-4">
      {error && (
        <p className="rounded-sm border border-danger bg-danger-bg px-3 py-2 text-sm text-danger">
          {error}
        </p>
      )}
      {notice && !error && <p className="text-sm text-success">{notice}</p>}

      <section className="card overflow-hidden">
        <header className="flex items-center gap-3 border-b border-line px-4 py-2 text-sm">
          <span className="text-muted">Бэкапы ({backups.length})</span>
          <button
            type="button"
            className="btn btn-primary ml-auto"
            disabled={busy}
            onClick={() => void handleCreate()}
          >
            Снять бэкап
          </button>
        </header>

        {backups.length === 0 ? (
          <p className="px-4 py-6 text-center text-sm text-muted">Бэкапов пока нет</p>
        ) : (
          <table className="w-full text-sm">
            <thead className="text-muted">
              <tr className="border-b border-line">
                <th className="px-4 py-2 text-left font-normal">Когда</th>
                <th className="px-4 py-2 text-left font-normal">Пометка</th>
                <th className="px-4 py-2 text-right font-normal">Размер</th>
                <th className="px-4 py-2 text-left font-normal">Состояние</th>
                <th className="px-4 py-2" />
              </tr>
            </thead>
            <tbody>
              {backups.map((backup) => (
                <tr key={backup.id} className="border-b border-line last:border-0">
                  <td className="px-4 py-2">{formatDate(backup.created_at)}</td>
                  <td className="px-4 py-2 text-muted">{backup.note || "—"}</td>
                  <td className="px-4 py-2 text-right text-muted">
                    {backup.state === "done" ? formatSize(backup.size_bytes) : "—"}
                  </td>
                  <td className="px-4 py-2">
                    <BackupState state={backup.state} />
                  </td>
                  <td className="px-4 py-2">
                    <div className="flex justify-end gap-3">
                      {backup.state === "done" && (
                        <>
                          <button
                            type="button"
                            className="text-xs text-accent hover:underline"
                            onClick={() => void handleDownload(backup)}
                          >
                            Скачать
                          </button>
                          <button
                            type="button"
                            className="text-xs text-warning hover:underline disabled:text-faint"
                            // The API refuses a restore onto a running server;
                            // saying so here beats letting the click fail.
                            disabled={busy || running}
                            title={running ? "Сначала остановите сервер" : undefined}
                            onClick={() => void handleRestore(backup)}
                          >
                            Восстановить
                          </button>
                        </>
                      )}
                      <button
                        type="button"
                        className="text-xs text-danger hover:underline"
                        disabled={busy}
                        onClick={() => void handleDelete(backup)}
                      >
                        Удалить
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      <section className="card p-4">
        <h2 className="mb-3 text-sm text-muted">Расписание</h2>

        {draft && (
          <div className="grid gap-3">
            <label className="flex items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={draft.enabled}
                onChange={(e) => setDraft({ ...draft, enabled: e.target.checked })}
              />
              Снимать бэкапы по расписанию
            </label>

            <div className="flex flex-wrap items-center gap-2">
              <label htmlFor="backup-cron" className="w-32 text-sm text-muted">
                Cron
              </label>
              <input
                id="backup-cron"
                value={draft.cron}
                onChange={(e) => setDraft({ ...draft, cron: e.target.value })}
                className="field h-9 w-44 font-mono text-sm"
              />
              {PRESETS.map((preset) => (
                <button
                  key={preset.cron}
                  type="button"
                  className="text-xs text-accent hover:underline"
                  onClick={() => setDraft({ ...draft, cron: preset.cron })}
                >
                  {preset.label}
                </button>
              ))}
            </div>

            <div className="flex items-center gap-2">
              <label htmlFor="backup-keep" className="w-32 text-sm text-muted">
                Хранить последних
              </label>
              <input
                id="backup-keep"
                type="number"
                min={1}
                max={100}
                value={draft.keep_last}
                onChange={(e) => setDraft({ ...draft, keep_last: Number(e.target.value) })}
                className="field h-9 w-24 text-sm"
              />
              <span className="text-xs text-muted">старые удаляются автоматически</span>
            </div>

            <div className="flex items-center gap-3">
              <button
                type="button"
                className="btn btn-primary"
                disabled={!scheduleChanged || busy}
                onClick={() => void handleSaveSchedule()}
              >
                Сохранить расписание
              </button>
              {schedule?.next_run_at && schedule.enabled && (
                <span className="text-xs text-muted">
                  Следующий запуск: {formatDate(schedule.next_run_at)}
                </span>
              )}
              {schedule?.last_run_at && (
                <span className="text-xs text-faint">
                  Последний: {formatDate(schedule.last_run_at)}
                </span>
              )}
            </div>
          </div>
        )}
      </section>
    </div>
  );
}

function BackupState({ state }: { state: api.Backup["state"] }) {
  const label = {
    pending: "в очереди",
    running: "снимается…",
    done: "готов",
    failed: "не удался",
  }[state];

  const className = {
    pending: "text-muted",
    running: "text-warning",
    done: "text-success",
    failed: "text-danger",
  }[state];

  return <span className={`text-xs ${className}`}>{label}</span>;
}

function formatDate(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "—";
  return date.toLocaleString("ru-RU", { hour12: false });
}

function formatSize(bytes: number): string {
  const mb = bytes / (1024 * 1024);
  if (mb < 1024) return `${mb.toFixed(1)} МБ`;
  return `${(mb / 1024).toFixed(2)} ГБ`;
}
