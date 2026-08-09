/*
 * Typed client for the Mirocraft REST API.
 *
 * Everything the panel does goes through here; no component talks to fetch
 * directly, so authentication, error shape and the base path live in one file.
 */

const TOKEN_KEY = "mirocraft.token";

export const API_BASE = "/api/v1";

/* --- types, mirroring docs/API.md --- */

export interface User {
  id: string;
  email: string;
  role: "admin" | "user";
  theme: string;
  blocked: boolean;
  max_servers: number;
  max_ram_mb: number;
  max_disk_mb: number;
  created_at: string;
}

export interface Me extends User {
  scopes: string[];
}

export interface ServerMetrics {
  ram_used_mb: number;
  cpu_percent: number;
  uptime_seconds: number;
  players_online: number | null;
  players_max: number | null;
}

export type ServerStatus =
  | "creating"
  | "stopped"
  | "starting"
  | "running"
  | "stopping"
  | "crashed";

export interface Server {
  id: string;
  name: string;
  core: string;
  version: string;
  kind: string;
  status: ServerStatus;
  ram_mb: number;
  port: number;
  java_args: string;
  owner_id: string;
  auto_start: boolean;
  auto_restart: boolean;
  eula_accepted: boolean;
  /** The proxy this server sits behind, empty when it is reached directly. */
  proxy_id?: string;
  /** Bedrock clients may join this Java server, through Geyser and Floodgate. */
  crossplay?: boolean;
  /** Where those clients connect. Zero when crossplay is off. */
  bedrock_port?: number;
  created_at: string;
  metrics?: ServerMetrics | null;
}

export interface ConsoleLine {
  type: "line";
  ts: string;
  stream: "stdout" | "stderr";
  text: string;
}

export interface CustomTheme {
  id: string;
  schema: string;
  name: string;
  base: "dark" | "light";
  vars: Record<string, string>;
}

export interface BuiltinThemeInfo {
  id: string;
  name: string;
  base: "dark" | "light";
  preview: { bg: string; text: string; accent: string };
}

export interface Task {
  id: string;
  kind: string;
  server_id?: string;
  status: "queued" | "running" | "done" | "failed";
  progress: number;
  error?: string;
}

export interface ApiToken {
  id: string;
  name: string;
  scopes: string[];
  kind: string;
  expires_at: string | null;
  last_used_at: string | null;
  created_at: string;
}

interface ListResponse<T> {
  items: T[];
  next_cursor: string | null;
}

/* --- errors --- */

/**
 * ApiError carries the machine-readable code, which is what callers should
 * branch on; the message is for humans and may change.
 */
export class ApiError extends Error {
  code: string;
  status: number;
  details?: Record<string, unknown>;

  constructor(status: number, code: string, message: string, details?: Record<string, unknown>) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.details = details;
  }
}

/* --- token storage --- */

export function getToken(): string | null {
  try {
    return localStorage.getItem(TOKEN_KEY);
  } catch {
    return null;
  }
}

export function setToken(token: string | null): void {
  try {
    if (token) localStorage.setItem(TOKEN_KEY, token);
    else localStorage.removeItem(TOKEN_KEY);
  } catch {
    // Private mode: the session simply does not survive a reload.
  }
}

/* --- request plumbing --- */

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers);
  headers.set("Accept", "application/json");
  if (init.body) headers.set("Content-Type", "application/json");

  const token = getToken();
  if (token) headers.set("Authorization", `Bearer ${token}`);

  const response = await fetch(API_BASE + path, { ...init, headers });

  if (response.status === 204) return undefined as T;

  const text = await response.text();
  const body = text ? JSON.parse(text) : null;

  if (!response.ok) {
    const error = body?.error ?? {};
    throw new ApiError(
      response.status,
      error.code ?? "internal_error",
      error.message ?? response.statusText,
      error.details,
    );
  }

  return body as T;
}

/* --- auth --- */

export async function login(email: string, password: string): Promise<{ token: string; user: User }> {
  const body = await request<{ token: string; expires_at: string; user: User }>("/auth/login", {
    method: "POST",
    body: JSON.stringify({ email, password }),
  });
  setToken(body.token);
  return body;
}

export async function logout(): Promise<void> {
  try {
    await request<void>("/auth/logout", { method: "POST" });
  } finally {
    // The local session is dropped even if the server call fails, or a user
    // trying to log out on a flaky connection stays logged in.
    setToken(null);
  }
}

export function me(): Promise<Me> {
  return request<Me>("/users/me");
}

export function updateMe(patch: {
  email?: string;
  password?: string;
  old_password?: string;
  theme?: string;
}): Promise<User> {
  return request<User>("/users/me", { method: "PATCH", body: JSON.stringify(patch) });
}

/* --- servers --- */

export async function listServers(): Promise<Server[]> {
  const body = await request<ListResponse<Server>>("/servers");
  return body.items;
}

export function getServer(id: string): Promise<Server> {
  return request<Server>(`/servers/${id}`);
}

export function createServer(input: {
  name: string;
  core: string;
  version: string;
  ram_mb: number;
  port?: number;
  eula_accepted: boolean;
}): Promise<Server> {
  return request<Server>("/servers", { method: "POST", body: JSON.stringify(input) });
}

export function deleteServer(id: string, name: string): Promise<void> {
  return request<void>(`/servers/${id}?confirm=${encodeURIComponent(name)}`, { method: "DELETE" });
}

export function power(
  id: string,
  action: "start" | "stop" | "restart" | "kill",
): Promise<{ task_id: string }> {
  return request<{ task_id: string }>(`/servers/${id}/power`, {
    method: "POST",
    body: JSON.stringify({ action }),
  });
}

export function getTask(id: string): Promise<Task> {
  return request<Task>(`/tasks/${id}`);
}

/* --- console --- */

export async function consoleHistory(id: string, lines = 500): Promise<ConsoleLine[]> {
  const body = await request<ListResponse<ConsoleLine>>(
    `/servers/${id}/console/history?lines=${lines}`,
  );
  return body.items;
}

export function sendCommand(id: string, command: string): Promise<void> {
  return request<void>(`/servers/${id}/command`, {
    method: "POST",
    body: JSON.stringify({ command }),
  });
}

export function consoleTicket(id: string): Promise<{ ticket: string; expires_at: string }> {
  return request<{ ticket: string; expires_at: string }>(`/servers/${id}/console/ticket`, {
    method: "POST",
  });
}

/**
 * consoleSocket opens the console stream. The ticket is fetched first so the
 * long-lived token never appears in a URL, where it would land in proxy logs
 * and browser history.
 */
export async function consoleSocket(id: string): Promise<WebSocket> {
  const { ticket } = await consoleTicket(id);
  const scheme = window.location.protocol === "https:" ? "wss" : "ws";
  const url = `${scheme}://${window.location.host}${API_BASE}/servers/${id}/console?token=${encodeURIComponent(ticket)}`;
  return new WebSocket(url);
}

/* --- files --- */

export interface FileEntry {
  name: string;
  path: string;
  type: "file" | "directory" | "symlink";
  size: number;
  mode: string;
  modified_at: string;
}

export async function listFiles(id: string, path = ""): Promise<FileEntry[]> {
  const body = await request<{ path: string; items: FileEntry[] }>(
    `/servers/${id}/files?path=${encodeURIComponent(path)}`,
  );
  return body.items;
}

export async function readFile(id: string, path: string): Promise<string> {
  const body = await request<{ path: string; content: string }>(
    `/servers/${id}/files/content?path=${encodeURIComponent(path)}`,
  );
  return body.content;
}

export function writeFile(id: string, path: string, content: string): Promise<void> {
  return request<void>(`/servers/${id}/files/content?path=${encodeURIComponent(path)}`, {
    method: "PUT",
    body: JSON.stringify({ content }),
  });
}

export function deleteFile(id: string, path: string): Promise<void> {
  return request<void>(`/servers/${id}/files?path=${encodeURIComponent(path)}`, {
    method: "DELETE",
  });
}

export function makeDirectory(id: string, path: string): Promise<void> {
  return request<void>(`/servers/${id}/files/mkdir`, {
    method: "POST",
    body: JSON.stringify({ path }),
  });
}

export function movePath(id: string, from: string, to: string): Promise<void> {
  return request<void>(`/servers/${id}/files/move`, {
    method: "POST",
    body: JSON.stringify({ from, to }),
  });
}

export function unarchive(id: string, path: string, destination: string): Promise<void> {
  return request<void>(`/servers/${id}/files/unarchive`, {
    method: "POST",
    body: JSON.stringify({ path, destination }),
  });
}

/**
 * uploadFile posts multipart form data, so it bypasses request(): that helper
 * sets a JSON content type, and a multipart body needs the boundary the
 * browser generates.
 */
export async function uploadFile(id: string, dir: string, file: File): Promise<void> {
  const form = new FormData();
  form.append("path", dir);
  form.append("file", file);

  const headers = new Headers({ Accept: "application/json" });
  const token = getToken();
  if (token) headers.set("Authorization", `Bearer ${token}`);

  const response = await fetch(`${API_BASE}/servers/${id}/files/upload`, {
    method: "POST",
    headers,
    body: form,
  });

  if (!response.ok) {
    const text = await response.text();
    const error = text ? (JSON.parse(text).error ?? {}) : {};
    throw new ApiError(
      response.status,
      error.code ?? "internal_error",
      error.message ?? response.statusText,
    );
  }
}

/**
 * downloadFile fetches through the API client rather than pointing the browser
 * at the URL: the download endpoint needs an Authorization header, and a plain
 * link cannot carry one. Putting the token in the query string instead would
 * leave it in browser history.
 */
export async function downloadFile(id: string, path: string): Promise<void> {
  const headers = new Headers();
  const token = getToken();
  if (token) headers.set("Authorization", `Bearer ${token}`);

  const response = await fetch(
    `${API_BASE}/servers/${id}/files/download?path=${encodeURIComponent(path)}`,
    { headers },
  );
  if (!response.ok) {
    throw new ApiError(response.status, "download_failed", "Не удалось скачать файл");
  }

  const blob = await response.blob();
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = path.split("/").pop() ?? "file";
  document.body.appendChild(link);
  link.click();
  link.remove();
  URL.revokeObjectURL(url);
}

/* --- server.properties --- */

export interface Setting {
  key: string;
  kind: "string" | "int" | "bool" | "enum";
  default?: string;
  min?: number;
  max?: number;
  values?: string[];
  note?: string;
}

export interface Settings {
  values: Record<string, string>;
  schema: Setting[];
  /** Keys the panel owns, mapped to why; shown read-only. */
  managed: Record<string, string>;
  /** Set on a save when the server is running: it reads the file at startup. */
  restart_required?: boolean;
}

export function getSettings(id: string): Promise<Settings> {
  return request<Settings>(`/servers/${id}/settings`);
}

export function patchSettings(id: string, values: Record<string, string>): Promise<Settings> {
  return request<Settings>(`/servers/${id}/settings`, {
    method: "PATCH",
    body: JSON.stringify(values),
  });
}

/* --- backups --- */

export interface Backup {
  id: string;
  server_id: string;
  note: string;
  state: "pending" | "running" | "done" | "failed";
  size_bytes: number;
  created_at: string;
}

export interface BackupSchedule {
  cron: string;
  keep_last: number;
  enabled: boolean;
  last_run_at: string | null;
  next_run_at: string | null;
}

export async function listBackups(id: string): Promise<Backup[]> {
  const body = await request<ListResponse<Backup>>(`/servers/${id}/backups`);
  return body.items;
}

export function createBackup(id: string, note?: string): Promise<{ task_id: string }> {
  return request<{ task_id: string }>(`/servers/${id}/backups`, {
    method: "POST",
    body: JSON.stringify({ note: note ?? "" }),
  });
}

export function deleteBackup(id: string, backupId: string): Promise<void> {
  return request<void>(`/servers/${id}/backups/${backupId}`, { method: "DELETE" });
}

export function restoreBackup(id: string, backupId: string): Promise<{ task_id: string }> {
  return request<{ task_id: string }>(`/servers/${id}/backups/${backupId}/restore`, {
    method: "POST",
  });
}

export function getBackupSchedule(id: string): Promise<BackupSchedule> {
  return request<BackupSchedule>(`/servers/${id}/backups/schedule`);
}

export function putBackupSchedule(
  id: string,
  schedule: { cron: string; keep_last: number; enabled: boolean },
): Promise<BackupSchedule> {
  return request<BackupSchedule>(`/servers/${id}/backups/schedule`, {
    method: "PUT",
    body: JSON.stringify(schedule),
  });
}

/** Downloads through the client for the same reason files do: the endpoint
 *  needs an Authorization header, and a plain link cannot carry one. */
export async function downloadBackup(id: string, backupId: string, name: string): Promise<void> {
  const headers = new Headers();
  const token = getToken();
  if (token) headers.set("Authorization", `Bearer ${token}`);

  const response = await fetch(`${API_BASE}/servers/${id}/backups/${backupId}/download`, {
    headers,
  });
  if (!response.ok) {
    throw new ApiError(response.status, "download_failed", "Не удалось скачать бэкап");
  }

  const blob = await response.blob();
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = name;
  document.body.appendChild(link);
  link.click();
  link.remove();
  URL.revokeObjectURL(url);
}

/* --- server settings --- */

export function patchServer(
  id: string,
  patch: {
    name?: string;
    ram_mb?: number;
    port?: number;
    java_args?: string;
    auto_start?: boolean;
    auto_restart?: boolean;
    proxy_id?: string;
    crossplay?: boolean;
  },
): Promise<Server> {
  return request<Server>(`/servers/${id}`, { method: "PATCH", body: JSON.stringify(patch) });
}

/* --- administration --- */

export async function listUsers(): Promise<User[]> {
  const body = await request<ListResponse<User>>("/admin/users");
  return body.items;
}

export function createUser(input: {
  email: string;
  password: string;
  role: "user" | "admin";
  max_servers?: number;
  max_ram_mb?: number;
}): Promise<User> {
  return request<User>("/admin/users", { method: "POST", body: JSON.stringify(input) });
}

export function patchUser(
  id: string,
  patch: {
    role?: "user" | "admin";
    blocked?: boolean;
    password?: string;
    max_servers?: number;
    max_ram_mb?: number;
  },
): Promise<User> {
  return request<User>(`/admin/users/${id}`, { method: "PATCH", body: JSON.stringify(patch) });
}

export function deleteUser(id: string): Promise<void> {
  return request<void>(`/admin/users/${id}`, { method: "DELETE" });
}

/* --- add-on catalogue --- */

export interface CatalogProject {
  id: string;
  slug: string;
  title: string;
  description: string;
  type: string;
  downloads: number;
  icon_url?: string;
  categories?: string[];
  loaders?: string[];
  license?: string;
  server_side?: string;
  client_side?: string;
  body?: string;
}

export interface CatalogVersion {
  id: string;
  project_id: string;
  name: string;
  number: string;
  channel: "release" | "beta" | "alpha";
  loaders: string[];
  game_versions: string[];
  published_at?: string;
}

export interface ServerContent {
  /** Empty when the core takes no add-ons at all. */
  loader: string;
  dir: string;
  version: string;
}

export interface PlannedFile {
  project_id: string;
  project_title: string;
  version_id: string;
  version_name: string;
  file_name: string;
  size_bytes: number;
  dependency: boolean;
}

export interface InstallPlan {
  files: PlannedFile[];
  skipped?: { project_id: string; reason: string }[];
}

export interface InstalledAddon {
  file: string;
  name: string;
  size_bytes: number;
  enabled: boolean;
  modified_at: string;
}

export function serverContent(id: string): Promise<ServerContent> {
  return request<ServerContent>(`/servers/${id}/catalog`);
}

export function searchCatalog(params: {
  q: string;
  loader?: string;
  mc?: string;
  type?: string;
  limit?: number;
  offset?: number;
}): Promise<{ items: CatalogProject[]; total: number; offset: number }> {
  const query = new URLSearchParams({ q: params.q });
  if (params.loader) query.set("loader", params.loader);
  if (params.mc) query.set("mc", params.mc);
  if (params.type) query.set("type", params.type);
  if (params.limit) query.set("limit", String(params.limit));
  if (params.offset) query.set("offset", String(params.offset));

  return request<{ items: CatalogProject[]; total: number; offset: number }>(
    `/catalog/search?${query.toString()}`,
  );
}

export function catalogProject(
  pid: string,
  loader?: string,
  mc?: string,
): Promise<{ project: CatalogProject; versions: CatalogVersion[] }> {
  const query = new URLSearchParams();
  if (loader) query.set("loader", loader);
  if (mc) query.set("mc", mc);

  return request<{ project: CatalogProject; versions: CatalogVersion[] }>(
    `/catalog/projects/${encodeURIComponent(pid)}?${query.toString()}`,
  );
}

export function installAddon(
  id: string,
  input: { project_id: string; version_id?: string; dry_run?: boolean },
): Promise<{ task_id?: string; plan?: InstallPlan; files?: PlannedFile[] }> {
  return request<{ task_id?: string; plan?: InstallPlan; files?: PlannedFile[] }>(
    `/servers/${id}/catalog/install`,
    { method: "POST", body: JSON.stringify(input) },
  );
}

export async function listInstalled(id: string): Promise<InstalledAddon[]> {
  const body = await request<ListResponse<InstalledAddon>>(`/servers/${id}/installed`);
  return body.items;
}

export function toggleInstalled(
  id: string,
  file: string,
): Promise<{ file: string; enabled: boolean; restart_required: boolean }> {
  return request<{ file: string; enabled: boolean; restart_required: boolean }>(
    `/servers/${id}/installed/${encodeURIComponent(file)}/toggle`,
    { method: "POST" },
  );
}

export function deleteInstalled(id: string, file: string): Promise<void> {
  return request<void>(`/servers/${id}/installed/${encodeURIComponent(file)}`, {
    method: "DELETE",
  });
}

/* --- themes --- */

export async function listBuiltinThemes(): Promise<BuiltinThemeInfo[]> {
  const body = await request<ListResponse<BuiltinThemeInfo>>("/themes");
  return body.items;
}

export async function listCustomThemes(): Promise<CustomTheme[]> {
  const body = await request<ListResponse<CustomTheme>>("/users/me/themes");
  return body.items;
}

export function createCustomTheme(theme: {
  schema: string;
  name: string;
  base: string;
  vars: Record<string, string>;
}): Promise<CustomTheme> {
  return request<CustomTheme>("/users/me/themes", {
    method: "POST",
    body: JSON.stringify(theme),
  });
}

export function updateCustomTheme(
  id: string,
  theme: { schema: string; name: string; base: string; vars: Record<string, string> },
): Promise<CustomTheme> {
  return request<CustomTheme>(`/users/me/themes/${id}`, {
    method: "PATCH",
    body: JSON.stringify(theme),
  });
}

export function deleteCustomTheme(id: string): Promise<void> {
  return request<void>(`/users/me/themes/${id}`, { method: "DELETE" });
}

/* --- tokens --- */

export async function listTokens(): Promise<ApiToken[]> {
  const body = await request<ListResponse<ApiToken>>("/auth/tokens");
  return body.items;
}

export function createApiToken(
  name: string,
  scopes: string[],
): Promise<ApiToken & { token: string }> {
  return request<ApiToken & { token: string }>("/auth/tokens", {
    method: "POST",
    body: JSON.stringify({ name, scopes }),
  });
}

export function deleteApiToken(id: string): Promise<void> {
  return request<void>(`/auth/tokens/${id}`, { method: "DELETE" });
}

/* --- chat bots --- */

export type BotProvider = "discord" | "telegram";
export type BotStatus = "off" | "connecting" | "connected" | "failed";

export interface BotSettings {
  provider: BotProvider;
  /** A token has been saved. The token itself is never sent to the panel. */
  configured: boolean;
  /** The last four characters, so an operator can recognise their bot. */
  token_hint: string;
  enabled: boolean;
  running: boolean;
  status: BotStatus;
  error?: string;
  account?: string;
  updated_at: string;
}

export async function listBots(): Promise<BotSettings[]> {
  const body = await request<ListResponse<BotSettings>>("/admin/bots");
  return body.items;
}

/**
 * Saves a bot's settings.
 *
 * The token is optional: leaving it out keeps the stored one, so the switch
 * can be flipped without pasting the secret again.
 */
export function saveBot(
  provider: BotProvider,
  patch: { token?: string; enabled?: boolean },
): Promise<BotSettings> {
  return request<BotSettings>(`/admin/bots/${provider}`, {
    method: "PUT",
    body: JSON.stringify(patch),
  });
}

export function deleteBot(provider: BotProvider): Promise<void> {
  return request<void>(`/admin/bots/${provider}`, { method: "DELETE" });
}

/* --- linking a chat account --- */

export interface Integration {
  provider: BotProvider;
  external_id: string;
  created_at: string;
}

export async function listIntegrations(): Promise<Integration[]> {
  const body = await request<ListResponse<Integration>>("/integrations");
  return body.items;
}

export function createLinkCode(
  provider: BotProvider,
): Promise<{ code: string; expires_at: string }> {
  return request<{ code: string; expires_at: string }>(
    `/integrations/${provider}/code`,
    { method: "POST" },
  );
}

export function unlinkIntegration(provider: BotProvider): Promise<void> {
  return request<void>(`/integrations/${provider}`, { method: "DELETE" });
}

/* --- cores --- */

export interface Core {
  id: string;
  name: string;
  kind: string;
  runtime: string;
  /** Empty when the core takes no add-ons at all, as vanilla does. */
  loader?: string;
  content_dir?: string;
  /** The first server on a version waits for a compile rather than a download. */
  builds_locally?: boolean;
}

export interface CoreVersion {
  id: string;
  channel: "release" | "snapshot";
  java_major: number;
}

export async function listCores(): Promise<Core[]> {
  const body = await request<ListResponse<Core>>("/cores");
  return body.items;
}

export async function listCoreVersions(core: string): Promise<CoreVersion[]> {
  const body = await request<ListResponse<CoreVersion>>(`/cores/${core}/versions`);
  return body.items;
}
