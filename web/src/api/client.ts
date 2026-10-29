import type { Role } from "./types";

export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
  ) {
    super(message);
  }
}

const DEV_USER_KEY = "harness.devUser";

export function getDevUser(): string {
  if (!import.meta.env.DEV) return "";
  try {
    return localStorage.getItem(DEV_USER_KEY) ?? "";
  } catch {
    return "";
  }
}
export function setDevUser(u: string) {
  try {
    if (u) localStorage.setItem(DEV_USER_KEY, u);
    else localStorage.removeItem(DEV_USER_KEY);
  } catch {
    /* storage unavailable */
  }
}

/** Extra headers on every request: only the dev user switcher, never in production builds. */
export function authHeaders(): Record<string, string> {
  const u = getDevUser();
  return import.meta.env.DEV && u ? { "X-Harness-User": u } : {};
}

let redirecting = false;
/** 401: the session is gone; send the browser through the server's login. */
export function onUnauthorized() {
  window.dispatchEvent(new CustomEvent("harness:unauthorized"));
}
export function loginRedirect() {
  if (redirecting) return;
  redirecting = true;
  const next = location.pathname + location.search + location.hash;
  location.assign(`/auth/login?next=${encodeURIComponent(next)}`);
}

export async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = { ...authHeaders() };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const r = await fetch("/api" + path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
    credentials: "same-origin",
  });
  if (r.status === 401) {
    onUnauthorized();
    throw new ApiError(401, "unauthenticated", "Authentication required.");
  }
  if (r.status === 204) return undefined as T;
  const text = await r.text();
  let json: unknown = undefined;
  try {
    json = text ? JSON.parse(text) : undefined;
  } catch {
    /* not JSON */
  }
  if (!r.ok) {
    const e = (json as { error?: { code?: string; message?: string } } | undefined)?.error;
    throw new ApiError(r.status, e?.code ?? "error", e?.message ?? (r.statusText || `HTTP ${r.status}`));
  }
  return json as T;
}

export const api = {
  get: <T>(p: string) => request<T>("GET", p),
  post: <T>(p: string, b?: unknown) => request<T>("POST", p, b ?? {}),
  put: <T>(p: string, b?: unknown) => request<T>("PUT", p, b ?? {}),
  patch: <T>(p: string, b?: unknown) => request<T>("PATCH", p, b ?? {}),
  del: <T = void>(p: string) => request<T>("DELETE", p),
};

export async function downloadFile(threadId: string, path: string, name: string) {
  const r = await fetch(`/api/threads/${threadId}/files/download?path=${encodeURIComponent(path)}`, { headers: authHeaders() });
  if (!r.ok) throw new ApiError(r.status, "download", "Download failed.");
  const url = URL.createObjectURL(await r.blob());
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10000);
}

export const canWrite = (r?: Role) => r === undefined || r === "owner" || r === "member";
export const isOwner = (r?: Role) => r === "owner";

/** True for "this feature is not available on this server" answers. */
export const isUnsupported = (e: unknown) => e instanceof ApiError && (e.status === 404 || e.status === 501);
