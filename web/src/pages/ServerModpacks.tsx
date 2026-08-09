import { useCallback, useEffect, useRef, useState } from "react";

import * as api from "../lib/api";

interface Props {
  serverId: string;
  running: boolean;
}

/** How long a search waits after the last keystroke before it asks upstream. */
const SEARCH_DEBOUNCE_MS = 350;

/** How often the install task is asked how far it has got. */
const POLL_MS = 1500;

/**
 * Modpacks for one server.
 *
 * Deliberately its own tab rather than a filter on the add-on catalogue: a
 * plugin is added to a server, a modpack replaces one. It changes the core,
 * the Minecraft version and every mod in the directory, and an action that
 * does that should not sit behind the same button as installing WorldEdit.
 */
export function ServerModpacks({ serverId, running }: Props) {
  const [installed, setInstalled] = useState<api.InstalledModpack | null>(null);
  const [query, setQuery] = useState("");
  const [hits, setHits] = useState<api.CatalogProject[] | null>(null);
  const [searching, setSearching] = useState(false);
  const [busy, setBusy] = useState<string | null>(null);
  const [task, setTask] = useState<api.Task | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  // Guards against an older search landing after a newer one, which is what
  // makes a search box feel haunted.
  const searchSeq = useRef(0);

  const reloadInstalled = useCallback(async () => {
    try {
      setInstalled(await api.serverModpack(serverId));
    } catch (err) {
      setError(err instanceof api.ApiError ? err.message : "Не удалось прочитать модпак");
    }
  }, [serverId]);

  useEffect(() => {
    void reloadInstalled();
  }, [reloadInstalled]);

  useEffect(() => {
    if (query.trim().length < 2) {
      setHits(null);
      return;
    }

    const seq = ++searchSeq.current;
    setSearching(true);

    const timer = setTimeout(() => {
      void (async () => {
        try {
          // No loader filter: a modpack brings its own loader, so filtering by
          // the one the server has now would hide every pack worth installing.
          const result = await api.searchCatalog({ q: query.trim(), type: "modpack", limit: 20 });
          if (seq === searchSeq.current) setHits(result.items ?? []);
        } catch (err) {
          if (seq === searchSeq.current) {
            setError(err instanceof api.ApiError ? err.message : "Поиск не удался");
            setHits([]);
          }
        } finally {
          if (seq === searchSeq.current) setSearching(false);
        }
      })();
    }, SEARCH_DEBOUNCE_MS);

    return () => clearTimeout(timer);
  }, [query]);

  // An install is hundreds of files and several minutes; without progress it
  // is indistinguishable from a hung panel.
  useEffect(() => {
    if (!task || task.status === "done" || task.status === "failed") return;

    const timer = setInterval(() => {
      void (async () => {
        try {
          const latest = await api.getTask(task.id);
          setTask(latest);
          if (latest.status === "done") {
            setNotice("Модпак установлен. Можно запускать сервер.");
            await reloadInstalled();
          }
          if (latest.status === "failed") {
            setError(latest.error ?? "Установка не удалась");
          }
        } catch {
          // A poll that fails is not the install failing; the next tick
          // asks again.
        }
      })();
    }, POLL_MS);

    return () => clearInterval(timer);
  }, [task, reloadInstalled]);

  async function handleInstall(project: api.CatalogProject) {
    setBusy(project.id);
    setError(null);
    setNotice(null);

    try {
      // Asked first without installing: this replaces the core, the Minecraft
      // version and everything in the mods directory, and an operator should
      // see that before it happens rather than afterwards.
      const preview = await api.installModpack(serverId, {
        project_id: project.id,
        dry_run: true,
      });
      const plan = (preview.plan ?? preview) as api.ModpackPlan;

      const lines = [
        `${project.title} ${plan.version || ""}`.trim(),
        plan.changes_core
          ? `Сервер переедет на ${plan.core} ${plan.minecraft}.`
          : `Ядро останется ${plan.core} ${plan.minecraft}.`,
        `Каталог ${plan.replaces_dir}/ будет очищен полностью — моды пака это весь список модов.`,
        "Мир и настройки останутся. Отменить установку нельзя; если сомневаетесь, сделайте бэкап.",
        "",
        "Устанавливаем?",
      ];
      if (!window.confirm(lines.join("\n"))) return;

      const started = await api.installModpack(serverId, {
        project_id: project.id,
        version_id: plan.version_id,
      });
      if (started.task_id) {
        setTask({
          id: started.task_id,
          kind: "modpack.install",
          status: "running",
          progress: 0,
        });
      }
      setNotice(`${project.title} устанавливается…`);
    } catch (err) {
      setError(err instanceof api.ApiError ? err.message : "Установка не удалась");
    } finally {
      setBusy(null);
    }
  }

  const installing = task !== null && (task.status === "queued" || task.status === "running");

  return (
    <div className="grid gap-4">
      {error && (
        <p className="rounded-sm border border-danger bg-danger-bg px-3 py-2 text-sm text-danger">
          {error}
        </p>
      )}
      {notice && !error && <p className="text-sm text-success">{notice}</p>}

      <section className="card p-4">
        {installed ? (
          <div className="grid gap-1 text-sm">
            <div className="flex items-center gap-2">
              <span className="font-medium">{installed.name}</span>
              {installed.version && <span className="text-xs text-faint">{installed.version}</span>}
            </div>
            <p className="text-muted">
              {installed.core} · Minecraft {installed.minecraft} · {installed.files}{" "}
              {pluralFiles(installed.files)}
            </p>
            <p className="text-xs text-faint">установлен {formatDate(installed.installed_at)}</p>
          </div>
        ) : (
          <p className="text-sm text-muted">
            Модпак не установлен. Установка модпака сама поставит нужный загрузчик и версию
            Minecraft — выбирать ядро вручную не нужно.
          </p>
        )}
      </section>

      {running && (
        <p className="rounded-sm border border-warning bg-warning-bg px-3 py-2 text-sm text-warning">
          Сервер запущен. Остановите его: установка стирает каталог модов и меняет ядро, а
          запущенный сервер держит эти файлы открытыми.
        </p>
      )}

      {installing && task && (
        <section className="card p-4">
          <p className="text-sm text-muted">
            Устанавливаем модпак… {task.progress}%. Скачиваются сотни файлов, это занимает
            несколько минут. Страницу можно закрыть — установка идёт на сервере.
          </p>
          <div className="mt-2 h-1 w-full rounded-sm bg-elevated">
            <div
              className="h-1 rounded-sm bg-accent transition-all"
              style={{ width: `${Math.max(task.progress, 2)}%` }}
            />
          </div>
        </section>
      )}

      <section className="card overflow-hidden">
        <header className="flex flex-wrap items-center gap-3 border-b border-line px-4 py-2 text-sm">
          <span className="text-muted">Поиск модпаков</span>
          <input
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="Например: All the Mods, Better MC"
            className="field h-8 w-64 py-1 text-sm"
          />
          {searching && <span className="text-xs text-muted">ищем…</span>}
          <span className="ml-auto text-xs text-faint">Modrinth</span>
        </header>

        {hits === null ? (
          <p className="px-4 py-6 text-center text-sm text-muted">
            Введите хотя бы два символа. Показываем только паки, которые ставятся на сервер.
          </p>
        ) : hits.length === 0 ? (
          <p className="px-4 py-6 text-center text-sm text-muted">Ничего не найдено</p>
        ) : (
          <ul className="divide-y divide-line">
            {hits.map((project) => (
              <li key={project.id} className="flex items-start gap-3 px-4 py-3">
                {project.icon_url ? (
                  // Straight from the registry's CDN, with no referrer: the
                  // panel's own address is often a private hostname, and it
                  // has no business travelling with every icon.
                  <img
                    src={project.icon_url}
                    alt=""
                    referrerPolicy="no-referrer"
                    className="h-10 w-10 shrink-0 rounded-sm"
                    loading="lazy"
                  />
                ) : (
                  <div className="h-10 w-10 shrink-0 rounded-sm bg-elevated" />
                )}

                <div className="min-w-0 flex-1">
                  <div className="flex items-center gap-2">
                    <span className="font-medium">{project.title}</span>
                    <span className="text-xs text-faint">
                      {formatDownloads(project.downloads)} загрузок
                    </span>
                  </div>
                  <p className="truncate text-sm text-muted">{project.description}</p>
                </div>

                <button
                  type="button"
                  className="btn btn-primary shrink-0"
                  disabled={busy !== null || installing || running}
                  onClick={() => void handleInstall(project)}
                >
                  {busy === project.id ? "…" : "Установить"}
                </button>
              </li>
            ))}
          </ul>
        )}
      </section>

      <p className="text-xs text-faint">
        CurseForge не поддерживается: их API требует ключ на каждую загрузку, а self-hosted панели
        неоткуда взять ключ для каждого оператора. Пак с CurseForge можно распаковать вручную через
        вкладку «Файлы».
      </p>
    </div>
  );
}

function pluralFiles(count: number): string {
  const tail = count % 10;
  const hundred = count % 100;
  if (tail === 1 && hundred !== 11) return "файл";
  if (tail >= 2 && tail <= 4 && (hundred < 12 || hundred > 14)) return "файла";
  return "файлов";
}

function formatDownloads(count: number): string {
  if (count < 1000) return String(count);
  if (count < 1_000_000) return `${(count / 1000).toFixed(0)} тыс.`;
  return `${(count / 1_000_000).toFixed(1)} млн`;
}

function formatDate(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString();
}
