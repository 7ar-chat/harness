import { useCallback, useEffect, useState } from "react";
import type { RunSettings } from "@/chat/useChat";

/** State mirrored to localStorage (per-viewer convenience only; never required). */
export function useStored<T>(key: string, initial: T): [T, (v: T) => void] {
  const [v, setV] = useState<T>(() => {
    try {
      const raw = localStorage.getItem(key);
      return raw === null ? initial : (JSON.parse(raw) as T);
    } catch {
      return initial;
    }
  });
  const set = useCallback(
    (n: T) => {
      setV(n);
      try {
        localStorage.setItem(key, JSON.stringify(n));
      } catch {
        /* storage unavailable */
      }
    },
    [key],
  );
  return [v, set];
}

export function useRunSettings(defaultModel?: string): [RunSettings, (p: Partial<RunSettings>) => void] {
  const [s, setS] = useStored<RunSettings>("harness.run", { interactive: true, mode: "build" });
  const merged: RunSettings = { ...s, model: s.model || defaultModel };
  return [merged, (p) => setS({ ...s, ...p })];
}

/** Size of an element, for libraries that need explicit pixel dimensions. Use the returned ref callback. */
export function useElementSize<T extends HTMLElement>() {
  const [el, setEl] = useState<T | null>(null);
  const [size, setSize] = useState({ width: 0, height: 0 });
  useEffect(() => {
    if (!el) return;
    const ro = new ResizeObserver(([e]) => e && setSize({ width: Math.floor(e.contentRect.width), height: Math.floor(e.contentRect.height) }));
    ro.observe(el);
    return () => ro.disconnect();
  }, [el]);
  return [setEl, size] as const;
}
