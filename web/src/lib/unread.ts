// Last-seen timestamps per thread (localStorage) drive the unread dots.
import { useSyncExternalStore } from "react";

const KEY = "harness.seen";
let cache: Record<string, string> | null = null;
const listeners = new Set<() => void>();

function load(): Record<string, string> {
  if (cache) return cache;
  try {
    cache = JSON.parse(localStorage.getItem(KEY) ?? "{}") as Record<string, string>;
  } catch {
    cache = {};
  }
  return cache;
}

export function markSeen(threadId: string, at: string = new Date().toISOString()) {
  const cur = load();
  if (cur[threadId] && cur[threadId] >= at) return;
  cache = { ...cur, [threadId]: at };
  try {
    localStorage.setItem(KEY, JSON.stringify(cache));
  } catch {
    /* storage unavailable */
  }
  listeners.forEach((l) => l());
}

export function useSeen(): Record<string, string> {
  return useSyncExternalStore(
    (l) => {
      listeners.add(l);
      return () => listeners.delete(l);
    },
    load,
  );
}

/** First visit to a thread we have never seen counts as seen: only later activity is "unread". */
export function isUnread(seen: Record<string, string>, id: string, updatedAt: string, openId?: string): boolean {
  if (id === openId) return false;
  const at = seen[id];
  return at !== undefined && updatedAt > at;
}
