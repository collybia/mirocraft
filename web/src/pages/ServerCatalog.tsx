import { useCallback, useEffect, useRef, useState } from "react";

import * as api from "../lib/api";

interface Props {
  serverId: string;
}

/** How long a search waits after the last keystroke before it asks upstream. */
const SEARCH_DEBOUNCE_MS = 350;

/**
 * The add-on catalogue for one server.
 *
 * Search is scoped to the server's own loader and Minecraft version rather
 * than left open: a Fabric mod listed for a Paper server is an install that
 * fails at the last step, and filtering upstream is both faster and honest.
 */
export function ServerCatalog({ serverId }: Props) {
  const [content, setContent] = useState<api.ServerContent | null>(null);
  const [query, setQuery] = useState("");
  const [hits, setHits] = useState<api.CatalogProject[] | null>(null);
  const [searching, setSearching] = useState(false);
  const [installed, setInstalled] = useState<api.InstalledAddon[]>([]);
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  // Guards against an older search landing after a newer one and overwriting
  // its results, which is what makes a search box feel haunted.
  const searchSeq = useRef(0);

  const reloadInstalled = useCallback(async () => {
    try {
      setInstalled(await api.listInstalled(serverId));
    } catch (err) {
      setError(err instanceof api.ApiError ? err.message : "Не удалось прочитать установленное");
    }
  }, [serverId]);

  useEffect(() => {
    let cancelled = false;

    void (async () => {
      try {
        const info = await api.serverContent(serverId);
        if (!cancelled) setContent(info);
      } catch (err) {
        if (!cancelled) {
          setError(err instanceof api.ApiError ? err.message : "Не удалось определить загрузчик");
        }
      }
    })();
    void reloadInstalled();

    return () => {
      cancelled = true;
    };
  }, [serverId, reloadInstalled]);

  useEffect(() => {
    if (!content?.loader || query.trim().length < 2) {
      setHits(null);
      return;
    }

    const seq = ++searchSeq.current;
    setSearching(true);

    const timer = setTimeout(() => {
      void (async () => {
        try {
          const result = await api.searchCatalog({
            q: query.trim(),
            loader: content.loader,
            mc: content.version,
            limit: 20,
          });
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
  }, [query, content]);

  async function handleInstall(project: api.CatalogProject) {
    setBusy(project.id);
    setError(null);
    setNotice(null);

    try {
      // Asked first without installing, so the count of files is known before
      // anything is downloaded: one click can bring four jars, and an operator
      // should see that rather than discover it.
      const preview = await api.installAddon(serverId, {
        project_id: project.id,
        dry_run: true,
      });
      const files = preview.files ?? preview.plan?.files ?? [];
      const extra = files.filter((f) => f.dependency);

      if (extra.length > 0) {
        const names = extra.map((f) => f.project_title).join(", ");
        if (!window.confirm(`${project.title} требует ещё: ${names}. Установить всё?`)) {
          return;
        }
      }

      await api.installAddon(serverId, { project_id: project.id });
      setNotice(
        extra.length > 0
          ? `${project.title} и ${extra.length} зависимост${extra.length === 1 ? "ь" : "и"} устанавливаются…`
          : `${project.title} устанавливается…`,
      );

      // The install is a task, so the file appears a moment later.
      setTimeout(() => void reloadInstalled(), 1500);
      setTimeout(() => void reloadInstalled(), 5000);
    } catch (err) {
      setError(err instanceof api.ApiError ? err.message : "Установка не удалась");
    } finally {
      setBusy(null);
    }
  }

  async function handleToggle(addon: api.InstalledAddon) {
    setBusy(addon.file);
    setError(null);
    try {
      const result = await api.toggleInstalled(serverId, addon.file);
      setNotice(
        result.restart_required
          ? `${addon.name} ${result.enabled ? "включён" : "выключен"}; сервер запущен — нужен перезапуск`
          : `${addon.name} ${result.enabled ? "включён" : "выключен"}`,
      );
      await reloadInstalled();
    } catch (err) {
      setError(err instanceof api.ApiError ? err.message : "Не удалось переключить");
    } finally {
      setBusy(null);
    }
  }

  async function handleDelete(addon: api.InstalledAddon) {
    if (!window.confirm(`Удалить ${addon.name}? Отменить будет нельзя.`)) return;

    setBusy(addon.file);
    setError(null);
    try {
      await api.deleteInstalled(serverId, addon.file);
      await reloadInstalled();
    } catch (err) {
      setError(err instanceof api.ApiError ? err.message : "Не удалось удалить");
    } finally {
      setBusy(null);
    }
  }

  if (content && !content.loader) {
    return (
      <section className="card p-4">
        <p className="text-sm text-muted">
          Это ядро не загружает плагины и моды. Jar рядом с ним просто не будет прочитан —
          чтобы ставить дополнения, создайте сервер на ядре, которое их поддерживает,
          например Paper.
        </p>
      </section>
    );
  }

  const installedNames = new Set(installed.map((addon) => addon.name.toLowerCase()));

  return (
    <div className="grid gap-4">
      {error && (
        <p className="rounded-sm border border-danger bg-danger-bg px-3 py-2 text-sm text-danger">
          {error}
        </p>
      )}
      {notice && !error && <p className="text-sm text-success">{notice}</p>}

      <section className="card overflow-hidden">
        <header className="flex flex-wrap items-center gap-3 border-b border-line px-4 py-2 text-sm">
          <span className="text-muted">Поиск дополнений</span>
          <input
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="Например: EssentialsX, WorldEdit"
            className="field h-8 w-64 py-1 text-sm"
          />
          {content && (
            <span className="text-xs text-faint">
              {content.loader} · {content.version} · {content.dir}/
            </span>
          )}
          {searching && <span className="text-xs text-muted">ищем…</span>}
        </header>

        {hits === null ? (
          <p className="px-4 py-6 text-center text-sm text-muted">
            Введите хотя бы два символа. Ищем только то, что подходит этому серверу.
          </p>
        ) : hits.length === 0 ? (
          <p className="px-4 py-6 text-center text-sm text-muted">
            Ничего не найдено для {content?.loader} {content?.version}
          </p>
        ) : (
          <ul className="divide-y divide-line">
            {hits.map((project) => (
              <li key={project.id} className="flex items-start gap-3 px-4 py-3">
                {project.icon_url ? (
                  // Loaded straight from the registry's CDN rather than
                  // proxied: the browser is already online if the search
                  // returned anything. no-referrer so the panel's own address —
                  // which on a home server is often a private hostname — does
                  // not travel to a third party with every icon.
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
                    {project.license && (
                      <span className="text-xs text-faint">{project.license}</span>
                    )}
                  </div>
                  <p className="truncate text-sm text-muted">{project.description}</p>
                </div>

                <button
                  type="button"
                  className="btn btn-primary shrink-0"
                  disabled={busy !== null}
                  onClick={() => void handleInstall(project)}
                >
                  {busy === project.id
                    ? "…"
                    : installedNames.has(project.title.toLowerCase())
                      ? "Переустановить"
                      : "Установить"}
                </button>
              </li>
            ))}
          </ul>
        )}
      </section>

      <section className="card overflow-hidden">
        <header className="border-b border-line px-4 py-2 text-sm text-muted">
          Установлено ({installed.length})
        </header>

        {installed.length === 0 ? (
          <p className="px-4 py-6 text-center text-sm text-muted">Пока ничего не установлено</p>
        ) : (
          <ul className="divide-y divide-line">
            {installed.map((addon) => (
              <li key={addon.file} className="flex items-center gap-3 px-4 py-2">
                <span className={addon.enabled ? "" : "text-muted line-through"}>{addon.name}</span>
                {!addon.enabled && <span className="text-xs text-warning">выключен</span>}
                <span className="text-xs text-faint">{formatSizeMb(addon.size_bytes)}</span>

                <div className="ml-auto flex gap-3">
                  <button
                    type="button"
                    className="text-xs text-accent hover:underline"
                    disabled={busy !== null}
                    onClick={() => void handleToggle(addon)}
                  >
                    {addon.enabled ? "Выключить" : "Включить"}
                  </button>
                  <button
                    type="button"
                    className="text-xs text-danger hover:underline"
                    disabled={busy !== null}
                    onClick={() => void handleDelete(addon)}
                  >
                    Удалить
                  </button>
                </div>
              </li>
            ))}
          </ul>
        )}
      </section>
    </div>
  );
}

function formatDownloads(count: number): string {
  if (count < 1000) return String(count);
  if (count < 1_000_000) return `${(count / 1000).toFixed(0)} тыс.`;
  return `${(count / 1_000_000).toFixed(1)} млн`;
}

function formatSizeMb(bytes: number): string {
  const mb = bytes / (1024 * 1024);
  return mb < 0.1 ? `${(bytes / 1024).toFixed(0)} КБ` : `${mb.toFixed(1)} МБ`;
}
