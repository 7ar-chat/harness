// The editor area. The tabs used most recently stay mounted (the 8 latest), the others unmount: scroll position, form drafts and the
// composer text survive a switch. A hidden tab keeps its layout (visibility, not display, so scroll offsets stay) but is out of the
// accessibility tree, inert and unclickable. Places the strip does not know (not found) fall through to the router's outlet.
import { Outlet } from "@tanstack/react-router";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { AdminPage } from "@/pages/Admin";
import { Dashboard } from "@/pages/Dashboard";
import { InboxPage } from "@/pages/Inbox";
import { JobsPage } from "@/pages/Jobs";
import { MemoryPage } from "@/pages/Memory";
import { CapabilitiesPage, ConnectorsPage, ProfilePage, ReferencesPage } from "@/pages/Profile";
import { ProjectPage } from "@/pages/ProjectPage";
import { SchedulesPage } from "@/pages/Schedules";
import { SessionTab, SupervisorPage } from "@/pages/SessionPage";
import { UsagePage } from "@/pages/Usage";
import { PlaceCtx, placeOf } from "@/lib/place";
import { TabActiveProvider } from "@/lib/tabActive";
import { ErrorBoundary } from "../ErrorBoundary";
import type { EditorTab, Tabs } from "./tabs";

export const KEEP_ALIVE = 8;

const PAGES: Record<string, () => ReactNode> = {
  "/supervisor": () => <SupervisorPage />,
  "/inbox": () => <InboxPage />,
  "/jobs": () => <JobsPage />,
  "/references": () => <ReferencesPage />,
  "/connectors": () => <ConnectorsPage />,
  "/capabilities": () => <CapabilitiesPage />,
  "/schedules": () => <SchedulesPage />,
  "/usage": () => <UsagePage />,
  "/memory": () => <MemoryPage />,
  "/profile": () => <ProfilePage />,
  "/admin": () => <AdminPage />,
};

function TabPage({ tab }: { tab: EditorTab }) {
  if (tab.kind === "home") return <Dashboard />;
  if (tab.kind === "session") return <SessionTab threadId={tab.id ?? ""} />;
  if (tab.kind === "page") return <>{PAGES[tab.href]?.()}</>;
  return (
    <PlaceCtx.Provider value={placeOf(tab.href)}>
      <ProjectPage />
    </PlaceCtx.Provider>
  );
}

export function TabHost({ tabs }: { tabs: Tabs }) {
  const active = tabs.active;
  // mount order (stable, so the DOM never reorders) and the last time each tab was shown (decides who is dropped)
  const [mounted, setMounted] = useState<EditorTab[]>([]);
  const used = useRef(new Map<string, number>());
  const clock = useRef(0);
  if (active) used.current.set(active.key, ++clock.current);

  useEffect(() => {
    setMounted((list) => {
      const open = new Set(tabs.tabs.map((t) => t.key));
      let next = list.filter((t) => open.has(t.key)).map((t) => tabs.tabs.find((x) => x.key === t.key) ?? t);
      if (active && !next.some((t) => t.key === active.key)) next = [...next, active];
      if (next.length > KEEP_ALIVE) {
        const drop = new Set([...next].sort((a, b) => (used.current.get(a.key) ?? 0) - (used.current.get(b.key) ?? 0)).slice(0, next.length - KEEP_ALIVE).map((t) => t.key));
        next = next.filter((t) => !drop.has(t.key) || t.key === active?.key);
      }
      const same = next.length === list.length && next.every((t, i) => t === list[i]);
      return same ? list : next;
    });
  }, [tabs.tabs, active]);

  // the very first render of a place shows it at once, before the effect has recorded it
  const list = active && !mounted.some((t) => t.key === active.key) ? [...mounted, active] : mounted;
  if (!active) {
    return (
      <div role="main" aria-label="Editor" className="min-h-0 flex-1 overflow-hidden bg-background">
        <Outlet />
      </div>
    );
  }
  return (
    <div className="relative min-h-0 flex-1 overflow-hidden bg-background">
      {list.map((t) => {
        const on = t.key === active.key;
        const hidden = on
          ? {}
          : ({ "aria-hidden": true, inert: "", style: { visibility: "hidden", pointerEvents: "none" } } as Record<string, unknown>);
        return (
          <div key={t.key} role={on ? "main" : undefined} aria-label={on ? "Editor" : undefined} className="absolute inset-0 overflow-hidden" {...hidden}>
            <TabActiveProvider active={on}>
              <ErrorBoundary reset={t.href}>
                <TabPage tab={on ? active : t} />
              </ErrorBoundary>
            </TabActiveProvider>
          </div>
        );
      })}
    </div>
  );
}
