// Unread dots: a thread is unread when it changed after the caller's last visit. The last-seen map lives on
// the server (see useSeen in api/queries); the browser's local storage only backs servers without /api/seen.
const KEY = "harness.seen";

export function loadLocalSeen(): Record<string, string> {
  try {
    const v: unknown = JSON.parse(localStorage.getItem(KEY) ?? "{}");
    return v && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, string>) : {};
  } catch {
    return {};
  }
}

export function saveLocalSeen(seen: Record<string, string>) {
  try {
    localStorage.setItem(KEY, JSON.stringify(seen));
  } catch {
    /* storage unavailable */
  }
}

/** Instants compare by time, not text: the server and the browser format fractional seconds differently. */
const after = (a: string, b: string) => Date.parse(a) > Date.parse(b);

/** True when `at` moves the recorded visit forward: a visit never moves backwards. */
export function isNewerVisit(seen: Record<string, string>, id: string, at: string): boolean {
  const cur = seen[id];
  return cur === undefined || after(at, cur);
}

/** First visit to a thread we have never seen counts as seen: only later activity is "unread". */
export function isUnread(seen: Record<string, string>, id: string, updatedAt: string, openId?: string): boolean {
  if (id === openId) return false;
  const at = seen[id];
  return at !== undefined && after(updatedAt, at);
}
