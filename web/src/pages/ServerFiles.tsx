import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";

import * as api from "../lib/api";

/**
 * Files larger than this are not opened in the editor.
 *
 * Not a limit of the API — it will happily return more — but of what a browser
 * can render as one <textarea> without freezing the tab. A 40 MB latest.log
 * opened by accident should say so rather than hang.
 */
const MAX_EDITABLE_BYTES = 2 << 20;

/** Extensions worth offering to edit. Everything else downloads instead. */
const TEXT_EXTENSIONS = new Set([
  "txt", "log", "json", "yml", "yaml", "properties", "conf", "cfg", "ini",
  "toml", "md", "sh", "bat", "xml", "csv", "lock", "env", "mcmeta",
]);

interface Props {
  serverId: string;
}

export function ServerFiles({ serverId }: Props) {
  const [dir, setDir] = useState("");
  const [entries, setEntries] = useState<api.FileEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [editing, setEditing] = useState<{ path: string; content: string } | null>(null);
  const [saving, setSaving] = useState(false);

  const uploadRef = useRef<HTMLInputElement | null>(null);

  const load = useCallback(
    async (rawTarget: string) => {
      // The API answers with rooted paths ("/plugins/config.yml") and accepts
      // either form. Stored without the leading slash so the breadcrumbs do
      // not have to render the empty segment that splitting "/plugins" gives.
      const target = clean(rawTarget);

      setLoading(true);
      setError(null);
      try {
        const items = await api.listFiles(serverId, target);
        // Directories first, then by name: a server directory holds a handful
        // of folders among dozens of files, and looking for plugins/ is the
        // usual reason anyone opens this page.
        items.sort((a, b) => {
          if ((a.type === "directory") !== (b.type === "directory")) {
            return a.type === "directory" ? -1 : 1;
          }
          return a.name.localeCompare(b.name, "ru");
        });
        setEntries(items);
        setDir(target);
      } catch (err) {
        setError(err instanceof Error ? err.message : "Не удалось прочитать каталог");
      } finally {
        setLoading(false);
      }
    },
    [serverId],
  );

  useEffect(() => {
    void load("");
  }, [load]);

  function report(err: unknown, fallback: string) {
    setError(err instanceof api.ApiError ? err.message : fallback);
  }

  async function openEntry(entry: api.FileEntry) {
    if (entry.type === "directory") {
      void load(entry.path);
      return;
    }
    if (!isEditable(entry)) {
      await handleDownload(entry);
      return;
    }

    setError(null);
    try {
      const content = await api.readFile(serverId, entry.path);
      setEditing({ path: entry.path, content });
    } catch (err) {
      report(err, "Не удалось открыть файл");
    }
  }

  async function handleSave() {
    if (!editing) return;
    setSaving(true);
    setError(null);
    try {
      await api.writeFile(serverId, editing.path, editing.content);
      setNotice(`${editing.path} сохранён`);
      setEditing(null);
      void load(dir);
    } catch (err) {
      report(err, "Не удалось сохранить файл");
    } finally {
      setSaving(false);
    }
  }

  async function handleDownload(entry: api.FileEntry) {
    setError(null);
    try {
      await api.downloadFile(serverId, entry.path);
    } catch (err) {
      report(err, "Не удалось скачать файл");
    }
  }

  async function handleDelete(entry: api.FileEntry) {
    const what = entry.type === "directory" ? "каталог и всё внутри" : "файл";
    if (!window.confirm(`Удалить ${what} «${entry.name}»? Отменить будет нельзя.`)) return;

    setError(null);
    try {
      await api.deleteFile(serverId, entry.path);
      void load(dir);
    } catch (err) {
      report(err, "Не удалось удалить");
    }
  }

  async function handleRename(entry: api.FileEntry) {
    const name = window.prompt("Новое имя:", entry.name);
    if (!name || name === entry.name) return;

    setError(null);
    try {
      await api.movePath(serverId, entry.path, join(dir, name));
      void load(dir);
    } catch (err) {
      report(err, "Не удалось переименовать");
    }
  }

  async function handleMkdir() {
    const name = window.prompt("Имя нового каталога:");
    if (!name) return;

    setError(null);
    try {
      await api.makeDirectory(serverId, join(dir, name));
      void load(dir);
    } catch (err) {
      report(err, "Не удалось создать каталог");
    }
  }

  async function handleUpload(files: FileList | null) {
    if (!files || files.length === 0) return;

    setError(null);
    setNotice(null);
    try {
      for (const file of Array.from(files)) {
        await api.uploadFile(serverId, dir, file);
      }
      setNotice(files.length === 1 ? "Файл загружен" : `Загружено файлов: ${files.length}`);
      void load(dir);
    } catch (err) {
      report(err, "Не удалось загрузить");
    } finally {
      if (uploadRef.current) uploadRef.current.value = "";
    }
  }

  async function handleUnarchive(entry: api.FileEntry) {
    const target = window.prompt("Куда распаковать (пусто — сюда же):", dir);
    if (target === null) return;

    setError(null);
    try {
      await api.unarchive(serverId, entry.path, target);
      setNotice(`${entry.name} распакован`);
      void load(dir);
    } catch (err) {
      report(err, "Не удалось распаковать");
    }
  }

  return (
    <section className="card overflow-hidden">
      <header className="flex flex-wrap items-center gap-2 border-b border-line px-4 py-2 text-sm">
        <Breadcrumbs dir={dir} onNavigate={(target) => void load(target)} />

        <div className="ml-auto flex gap-2">
          <button type="button" className="btn btn-ghost" onClick={() => void handleMkdir()}>
            Новый каталог
          </button>
          <button
            type="button"
            className="btn btn-ghost"
            onClick={() => uploadRef.current?.click()}
          >
            Загрузить
          </button>
          <input
            ref={uploadRef}
            type="file"
            multiple
            className="hidden"
            onChange={(e) => void handleUpload(e.target.files)}
          />
        </div>
      </header>

      {error && (
        <p className="border-b border-line bg-danger-bg px-4 py-2 text-sm text-danger">{error}</p>
      )}
      {notice && !error && (
        <p className="border-b border-line px-4 py-2 text-sm text-success">{notice}</p>
      )}

      {editing ? (
        <Editor
          path={editing.path}
          content={editing.content}
          saving={saving}
          onChange={(content) => setEditing({ ...editing, content })}
          onSave={() => void handleSave()}
          onCancel={() => setEditing(null)}
        />
      ) : (
        <table className="w-full text-sm">
          <thead className="text-muted">
            <tr className="border-b border-line">
              <th className="px-4 py-2 text-left font-normal">Имя</th>
              <th className="px-4 py-2 text-right font-normal">Размер</th>
              <th className="px-4 py-2 text-left font-normal">Изменён</th>
              <th className="px-4 py-2" />
            </tr>
          </thead>
          <tbody>
            {dir !== "" && (
              <tr className="border-b border-line">
                <td colSpan={4} className="px-4 py-2">
                  <button
                    type="button"
                    className="text-accent hover:underline"
                    onClick={() => void load(parentOf(dir))}
                  >
                    ↑ наверх
                  </button>
                </td>
              </tr>
            )}

            {loading && (
              <tr>
                <td colSpan={4} className="px-4 py-6 text-center text-muted">
                  Загрузка…
                </td>
              </tr>
            )}

            {!loading && entries.length === 0 && (
              <tr>
                <td colSpan={4} className="px-4 py-6 text-center text-muted">
                  Каталог пуст
                </td>
              </tr>
            )}

            {!loading &&
              entries.map((entry) => (
                <tr key={entry.path} className="border-b border-line last:border-0">
                  <td className="px-4 py-2">
                    <button
                      type="button"
                      className="text-left hover:underline"
                      onClick={() => void openEntry(entry)}
                    >
                      <span className="text-muted">{entry.type === "directory" ? "📁" : "📄"}</span>{" "}
                      {entry.name}
                    </button>
                  </td>
                  <td className="px-4 py-2 text-right text-muted">
                    {entry.type === "directory" ? "—" : formatSize(entry.size)}
                  </td>
                  <td className="px-4 py-2 text-muted">{formatDate(entry.modified_at)}</td>
                  <td className="px-4 py-2">
                    <div className="flex justify-end gap-2">
                      {isArchive(entry) && (
                        <RowButton onClick={() => void handleUnarchive(entry)}>Распаковать</RowButton>
                      )}
                      {entry.type !== "directory" && (
                        <RowButton onClick={() => void handleDownload(entry)}>Скачать</RowButton>
                      )}
                      <RowButton onClick={() => void handleRename(entry)}>Переименовать</RowButton>
                      <RowButton danger onClick={() => void handleDelete(entry)}>
                        Удалить
                      </RowButton>
                    </div>
                  </td>
                </tr>
              ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

function Editor({
  path,
  content,
  saving,
  onChange,
  onSave,
  onCancel,
}: {
  path: string;
  content: string;
  saving: boolean;
  onChange: (value: string) => void;
  onSave: () => void;
  onCancel: () => void;
}) {
  return (
    <div>
      <div className="flex items-center gap-2 border-b border-line px-4 py-2 text-sm">
        <span className="font-mono">{path}</span>
        <div className="ml-auto flex gap-2">
          <button type="button" className="btn btn-ghost" onClick={onCancel} disabled={saving}>
            Отмена
          </button>
          <button type="button" className="btn btn-primary" onClick={onSave} disabled={saving}>
            {saving ? "Сохранение…" : "Сохранить"}
          </button>
        </div>
      </div>
      <textarea
        value={content}
        onChange={(e) => onChange(e.target.value)}
        spellCheck={false}
        className="field h-96 w-full rounded-none border-0 font-mono text-xs leading-relaxed"
      />
    </div>
  );
}

function Breadcrumbs({ dir, onNavigate }: { dir: string; onNavigate: (path: string) => void }) {
  const parts = clean(dir) === "" ? [] : clean(dir).split("/");

  return (
    <nav className="flex flex-wrap items-center gap-1 font-mono text-xs">
      <button type="button" className="text-accent hover:underline" onClick={() => onNavigate("")}>
        /
      </button>
      {parts.map((part, index) => {
        const target = parts.slice(0, index + 1).join("/");
        const last = index === parts.length - 1;
        return (
          <span key={target} className="flex items-center gap-1">
            <button
              type="button"
              className={last ? "text-body" : "text-accent hover:underline"}
              onClick={() => onNavigate(target)}
            >
              {part}
            </button>
            {!last && <span className="text-faint">/</span>}
          </span>
        );
      })}
    </nav>
  );
}

function RowButton({
  children,
  danger,
  onClick,
}: {
  children: ReactNode;
  danger?: boolean;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`text-xs hover:underline ${danger ? "text-danger" : "text-accent"}`}
    >
      {children}
    </button>
  );
}

/* --- helpers --- */

/** clean drops the leading slash the API uses, so "" means the root. */
function clean(path: string): string {
  return path.replace(/^\/+/, "");
}

function join(dir: string, name: string): string {
  return dir === "" ? name : `${dir}/${name}`;
}

function parentOf(dir: string): string {
  const index = clean(dir).lastIndexOf("/");
  return index < 0 ? "" : clean(dir).slice(0, index);
}

function extensionOf(name: string): string {
  const index = name.lastIndexOf(".");
  return index < 0 ? "" : name.slice(index + 1).toLowerCase();
}

function isEditable(entry: api.FileEntry): boolean {
  if (entry.type !== "file" || entry.size > MAX_EDITABLE_BYTES) return false;
  // A file with no extension in a server directory is usually text — eula.txt
  // aside, things like "banned-ips" and "ops" turn up without one.
  const ext = extensionOf(entry.name);
  return ext === "" || TEXT_EXTENSIONS.has(ext);
}

function isArchive(entry: api.FileEntry): boolean {
  if (entry.type !== "file") return false;
  const name = entry.name.toLowerCase();
  return name.endsWith(".zip") || name.endsWith(".tar.gz") || name.endsWith(".tgz");
}

function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} Б`;
  const kb = bytes / 1024;
  if (kb < 1024) return `${kb.toFixed(0)} КБ`;
  const mb = kb / 1024;
  if (mb < 1024) return `${mb.toFixed(1)} МБ`;
  return `${(mb / 1024).toFixed(1)} ГБ`;
}

function formatDate(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "—";
  return date.toLocaleString("ru-RU", { hour12: false });
}
