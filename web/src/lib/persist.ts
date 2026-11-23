// Small per-browser conveniences (open tabs, panel sizes). Storage can be blocked or full: every access is guarded, and the app
// works the same without it.
import { useCallback, useState } from "react";

export function readJSON<T>(key: string, fallback: T): T {
  try {
    const raw = localStorage.getItem(key);
    return raw === null ? fallback : (JSON.parse(raw) as T);
  } catch {
    return fallback;
  }
}

export function writeJSON(key: string, value: unknown) {
  try {
    localStorage.setItem(key, JSON.stringify(value));
  } catch {
    /* storage is a convenience */
  }
}

/** useState that remembers its value in localStorage under `key`. */
export function usePersisted<T>(key: string, initial: T): [T, (v: T | ((p: T) => T)) => void] {
  const [value, setValue] = useState<T>(() => readJSON(key, initial));
  const set = useCallback(
    (v: T | ((p: T) => T)) =>
      setValue((prev) => {
        const next = typeof v === "function" ? (v as (p: T) => T)(prev) : v;
        writeJSON(key, next);
        return next;
      }),
    [key],
  );
  return [value, set];
}
