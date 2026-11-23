// Shell state shared by the explorer, the editor tabs, the bottom panel, the status bar and the palette.
import { useRouter } from "@tanstack/react-router";
import { createContext, useCallback, useContext, useEffect, useState, type AnchorHTMLAttributes, type ReactNode } from "react";
import { cn } from "@/lib/utils";
import type { Tabs } from "./tabs";

export type PanelTab = "needs" | "activity" | "tasks" | "usage";

export interface ShellState {
  tabs: Tabs;
  /** The project the explorer shows (undefined: personal space only). */
  projectId?: string;
  setProjectId: (id?: string) => void;
  narrow: boolean;
  explorer: boolean;
  setExplorer: (v: boolean | ((p: boolean) => boolean)) => void;
  panel: boolean;
  setPanel: (v: boolean | ((p: boolean) => boolean)) => void;
  panelTab: PanelTab;
  setPanelTab: (t: PanelTab) => void;
  openSwitcher: () => void;
}

const Ctx = createContext<ShellState | null>(null);
export const ShellProvider = ({ value, children }: { value: ShellState; children: ReactNode }) => <Ctx.Provider value={value}>{children}</Ctx.Provider>;
export function useShell(): ShellState {
  const v = useContext(Ctx);
  if (!v) throw new Error("useShell outside the app shell");
  return v;
}

/** True below the width where the explorer becomes a drawer and the panels stack. */
export function useNarrow(max = 899) {
  const q = `(max-width: ${max}px)`;
  const [narrow, setNarrow] = useState(() => (typeof window !== "undefined" && window.matchMedia ? window.matchMedia(q).matches : false));
  useEffect(() => {
    const m = window.matchMedia(q);
    const h = () => setNarrow(m.matches);
    h();
    m.addEventListener?.("change", h);
    return () => m.removeEventListener?.("change", h);
  }, [q]);
  return narrow;
}

/** A real link (right-click, copy address, open in a new window work) that opens the place in the editor on a plain click. */
export function NavLink({ href, onNavigate, className, children, ...rest }: { href: string; onNavigate?: () => void } & Omit<AnchorHTMLAttributes<HTMLAnchorElement>, "href">) {
  const router = useRouter();
  const open = useCallback(
    (e: React.MouseEvent<HTMLAnchorElement>) => {
      rest.onClick?.(e);
      if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
      e.preventDefault();
      void router.navigate({ href });
      onNavigate?.();
    },
    [router, href, onNavigate, rest],
  );
  return (
    <a {...rest} href={router.history.createHref(href)} onClick={open} className={cn(className)}>
      {children}
    </a>
  );
}
