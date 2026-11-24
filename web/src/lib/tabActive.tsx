// Whether the place a component lives in is the one being looked at. The tab host keeps up to eight tabs mounted, hidden: a hidden tab
// must not poll or hold a stream open, and refetches what went stale when it is shown again. Outside a tab host (the shell, a test)
// everything counts as active.
import { useQueryClient } from "@tanstack/react-query";
import { createContext, useContext, useEffect, useRef, useSyncExternalStore, type ReactNode } from "react";

const TabActiveCtx = createContext(true);

function subscribeVisibility(cb: () => void) {
  document.addEventListener("visibilitychange", cb);
  return () => document.removeEventListener("visibilitychange", cb);
}
const pageVisible = () => document.visibilityState !== "hidden";

/** True while the browser tab is in the foreground. */
export function usePageVisible(): boolean {
  return useSyncExternalStore(subscribeVisibility, pageVisible, () => true);
}

/** True when the editor tab this component is in is the shown one (always true outside a tab host). */
export function useTabActive(): boolean {
  return useContext(TabActiveCtx);
}

/** The tab is shown and the browser tab is in the foreground: the only time polling and streaming make sense. */
export function useLive(): boolean {
  const tab = useTabActive();
  const page = usePageVisible();
  return tab && page;
}

/** A refetchInterval that is off while the tab is hidden: `refetchInterval: useLivePoll(15_000)`. */
export function useLivePoll(ms: number): number | false {
  return useLive() ? ms : false;
}

/** A refetchInterval function (see queue.tsx) that is off while the tab is hidden. */
export function useLivePollFn<Q>(fn: (q: Q) => number | false): (q: Q) => number | false {
  const live = useLive();
  return (q) => (live ? fn(q) : false);
}

/** Wraps one editor tab: its children see `active`, and the tab refetches its stale queries when it becomes shown. */
export function TabActiveProvider({ active, children }: { active: boolean; children: ReactNode }) {
  const qc = useQueryClient();
  const was = useRef(active);
  useEffect(() => {
    if (active && !was.current) void qc.refetchQueries({ type: "active", stale: true });
    was.current = active;
  }, [active, qc]);
  return <TabActiveCtx.Provider value={active}>{children}</TabActiveCtx.Provider>;
}
