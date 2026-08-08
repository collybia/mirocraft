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
