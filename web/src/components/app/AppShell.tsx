// The "Console" shell: an explorer on the left, editor tabs holding every page and session, a bottom panel and a status bar.
// The URL still decides what is shown (hash routes), the shell only keeps the list of open tabs around it.
import { useParams } from "@tanstack/react-router";
import { PanelLeft } from "lucide-react";
import { useCallback, useEffect, useMemo, useState } from "react";
import { useNeedsReview } from "@/api/factory";
import { useMe, useOverview } from "@/api/queries";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { identifyUser } from "@/lib/telemetry";
import { usePersisted } from "@/lib/persist";
import { DialogsProvider } from "./dialogs";
import { BottomPanel } from "./shell/BottomPanel";
import { ShellProvider, useNarrow, type PanelTab, type ShellState } from "./shell/context";
import { clamp, ResizeHandle } from "./shell/ResizeHandle";
import { EditorTabs } from "./shell/EditorTabs";
import { ExplorerBody } from "./shell/Explorer";
import { Palette, ProjectSwitcher } from "./shell/Palette";
import { TabHost } from "./shell/TabHost";
import { StatusBar } from "./shell/StatusBar";
import { useEditorTabs } from "./shell/tabs";

export function AppShell() {
  const [palette, setPalette] = useState(false);
  const [switcher, setSwitcher] = useState(false);
  const login = useMe().data?.user.login ?? "";
  // Usage statistics know a person by their login and nothing else; a no-op when statistics are off.
  useEffect(() => identifyUser(login), [login]);
  return (
    <DialogsProvider openPalette={() => setPalette(true)}>
      <Shell login={login} palette={palette} setPalette={setPalette} switcher={switcher} setSwitcher={setSwitcher} />
    </DialogsProvider>
  );
}

function Shell({ login, palette, setPalette, switcher, setSwitcher }: { login: string; palette: boolean; setPalette: (v: boolean | ((p: boolean) => boolean)) => void; switcher: boolean; setSwitcher: (v: boolean) => void }) {
  const narrow = useNarrow();
  const params = useParams({ strict: false }) as { threadId?: string; projectId?: string };
  const overview = useOverview().data;
  const tabs = useEditorTabs(login);

  // desktop: remembered; narrow: the explorer is a drawer and the panel a sheet, both closed until asked for
  const [explorerD, setExplorerD] = usePersisted("7ar.ui.explorer", true);
  // The panel is collapsed until something needs the person; an explicit open or close is remembered and wins from then on.
  const needsCount = useNeedsReview().data?.count ?? 0;
  const [panelChoice, setPanelChoice] = usePersisted<boolean | null>("7ar.ui.panel.v2", null);
  const panelD = panelChoice ?? needsCount > 0;
  const setPanelD = useCallback((v: boolean | ((p: boolean) => boolean)) => setPanelChoice(typeof v === "function" ? v(panelD) : v), [panelD, setPanelChoice]);
  const [explorerW, setExplorerW] = usePersisted("7ar.ui.explorerW", 272);
  const [panelH, setPanelH] = usePersisted("7ar.ui.panelH", 178);
  const [drawer, setDrawer] = useState(false);
  const [sheet, setSheet] = useState(false);
  const [panelTab, setPanelTabState] = usePersisted<PanelTab>("7ar.ui.panelTab", "needs");

  // the project the explorer shows: the one in the address, else the last one picked, else the first
  const [picked, setPicked] = usePersisted<string | null>(`7ar.project.${login}`, null);
  // A personal session (no project) is its own context: the explorer says "Personal" instead of showing some project.
  const sessionId = params.threadId ?? (tabs.active?.kind === "session" ? tabs.active.id : undefined);
  const personalSession = !!sessionId && !!overview?.personal.some((t) => t.id === sessionId) && !overview.projects.some((p) => p.threads.some((t) => t.id === sessionId));
  const threadProject = sessionId ? overview?.projects.find((p) => p.threads.some((t) => t.id === sessionId))?.id : undefined;
  const fromLocation = params.projectId ?? threadProject;
  useEffect(() => {
    if (fromLocation && fromLocation !== picked) setPicked(fromLocation);
  }, [fromLocation, picked, setPicked]);
  const projectId = useMemo(() => {
    if (fromLocation) return fromLocation;
    if (personalSession) return undefined;
    if (picked === "") return undefined;
    if (picked && overview?.projects.some((p) => p.id === picked)) return picked;
    return overview?.projects[0]?.id;
  }, [fromLocation, personalSession, picked, overview]);

  const setExplorer = useCallback((v: boolean | ((p: boolean) => boolean)) => (narrow ? setDrawer(v) : setExplorerD(v)), [narrow, setExplorerD]);
  const setPanel = useCallback((v: boolean | ((p: boolean) => boolean)) => (narrow ? setSheet(v) : setPanelD(v)), [narrow, setPanelD]);
  const state: ShellState = {
    tabs,
    projectId,
    setProjectId: (id) => setPicked(id ?? ""),
    narrow,
    explorer: narrow ? drawer : explorerD,
    setExplorer,
    panel: narrow ? sheet : panelD,
    needsCount,
    setPanel,
    panelTab,
    setPanelTab: setPanelTabState,
    openSwitcher: () => setSwitcher(true),
  };

  useEffect(() => {
    const h = (e: KeyboardEvent) => {
      const mod = e.metaKey || e.ctrlKey;
      const k = e.key.toLowerCase();
      if (mod && !e.altKey && k === "k") {
        e.preventDefault();
        setPalette((p) => !p);
      } else if (mod && !e.altKey && !e.shiftKey && k === "p") {
        e.preventDefault();
        setSwitcher(true);
      } else if (mod && !e.altKey && k === "j") {
        e.preventDefault();
        setPanel((p) => !p);
      } else if (mod && !e.altKey && k === "b") {
        e.preventDefault();
        setExplorer((p) => !p);
      } else if ((e.ctrlKey && !e.metaKey && k === "w") || (e.altKey && !mod && k === "w")) {
        if (tabs.active) {
          e.preventDefault();
          tabs.close(tabs.active.key);
        }
      } else if (e.ctrlKey && e.key === "Tab") {
        e.preventDefault();
        tabs.next(e.shiftKey ? -1 : 1);
      } else if (e.altKey && !mod && (e.key === "]" || e.key === "[")) {
        e.preventDefault();
        tabs.next(e.key === "]" ? 1 : -1);
      }
    };
    window.addEventListener("keydown", h);
    return () => window.removeEventListener("keydown", h);
  }, [tabs, setPalette, setSwitcher, setPanel, setExplorer]);

  const closeDrawer = () => setDrawer(false);
  return (
    <ShellProvider value={state}>
      <div className="flex h-svh min-h-0 flex-col overflow-hidden bg-shell text-foreground">
        <div className="flex min-h-0 flex-1">
          {!narrow && explorerD ? (
            <>
              <aside aria-label="Explorer panel" style={{ width: clamp(explorerW, 200, 480) }} className="flex shrink-0 flex-col border-r bg-sidebar">
                <ExplorerBody />
              </aside>
              <ResizeHandle orientation="vertical" label="Resize explorer" value={clamp(explorerW, 200, 480)} min={200} max={480} initial={272} onChange={setExplorerW} />
            </>
          ) : null}
          <div className="flex min-w-0 flex-1 flex-col">
            <div className="flex shrink-0 border-b bg-panel">
              {narrow ? (
                <button type="button" onClick={() => setDrawer(true)} aria-label="Open navigation" className="flex w-10 shrink-0 items-center justify-center border-r text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring">
                  <PanelLeft className="size-4" />
                </button>
              ) : null}
              <div className="min-w-0 flex-1">
                <EditorTabs tabs={tabs} />
              </div>
            </div>
            <TabHost tabs={tabs} />
          </div>
        </div>
        {!narrow && panelD ? (
          <>
            <ResizeHandle orientation="horizontal" label="Resize panel" value={clamp(panelH, 100, 480)} min={100} max={480} initial={178} onChange={setPanelH} className="border-t" />
            <div style={{ height: clamp(panelH, 100, 480) }} className="shrink-0">
              <BottomPanel onClose={() => setPanelD(false)} />
            </div>
          </>
        ) : null}
        <StatusBar />
      </div>
      {narrow ? (
        <>
          <Sheet open={drawer} onOpenChange={setDrawer}>
            <SheetContent side="left" className="w-[300px] max-w-[88vw] gap-0 bg-sidebar p-0">
              <SheetHeader className="sr-only">
                <SheetTitle>Navigation</SheetTitle>
                <SheetDescription>Projects, sessions and pages</SheetDescription>
              </SheetHeader>
              <ExplorerBody onNavigate={closeDrawer} />
            </SheetContent>
          </Sheet>
          <Sheet open={sheet} onOpenChange={setSheet}>
            <SheetContent side="bottom" className="h-[60vh] gap-0 p-0">
              <SheetHeader className="sr-only">
                <SheetTitle>Panel</SheetTitle>
                <SheetDescription>What needs you, activity, tasks and usage</SheetDescription>
              </SheetHeader>
              <BottomPanel />
            </SheetContent>
          </Sheet>
        </>
      ) : null}
      <Palette open={palette} setOpen={setPalette} />
      <ProjectSwitcher open={switcher} setOpen={setSwitcher} />
    </ShellProvider>
  );
}
