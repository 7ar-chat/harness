// The strip of open editor tabs: click or Enter focuses, middle-click or the x closes, drag reorders, the strip scrolls sideways.
import { X } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { useTask } from "@/api/factory";
import { useOverview } from "@/api/queries";
import { useWorkflows } from "@/api/workflows";
import { cn } from "@/lib/utils";
import { StatusDot } from "../common";
import { PROJECT_SECTIONS, pageTitle, type EditorTab, type Tabs } from "./tabs";

/** The words on a tab. Project places say which project when more than one project has a tab open. */
export function useTabLabel(tab: EditorTab, multi: boolean): string {
  const overview = useOverview().data;
  const task = useTask(tab.projectId ?? "", tab.kind === "task" ? tab.id : undefined).data?.task.title;
  const wfs = useWorkflows(tab.kind === "workflow" ? (tab.projectId ?? "") : "").data;
  const pname = overview?.projects.find((p) => p.id === tab.projectId)?.name;
  const suffix = multi && pname ? ` · ${pname}` : "";
  switch (tab.kind) {
    case "home":
      return "Dashboard";
    case "page":
      return pageTitle(tab.href) ?? tab.href;
    case "session": {
      const t = [...(overview?.personal ?? []), ...(overview?.projects ?? []).flatMap((p) => p.threads)].find((x) => x.id === tab.id);
      return t ? t.title || "(untitled)" : "Session";
    }
    case "task":
      return `Task · ${task ?? "…"}`;
    case "workflow":
      return `Workflow · ${wfs?.find((w) => w.id === tab.id)?.name ?? "…"}`;
    case "run":
      return `Run · ${(tab.id ?? "").slice(0, 8)}`;
    default:
      return `${PROJECT_SECTIONS[tab.section ?? "board"] ?? tab.section}${suffix}`;
  }
}

function TabButton({ tab, active, multi, tabs, dragging, setDragging }: { tab: EditorTab; active: boolean; multi: boolean; tabs: Tabs; dragging?: string; setDragging: (k?: string) => void }) {
  const label = useTabLabel(tab, multi);
  const overview = useOverview().data;
  const status = tab.kind === "session" ? [...(overview?.personal ?? []), ...(overview?.projects ?? []).flatMap((p) => p.threads)].find((x) => x.id === tab.id)?.status : undefined;
  const ref = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    if (active) ref.current?.scrollIntoView?.({ block: "nearest", inline: "nearest" });
  }, [active]);
  return (
    <div
      role="presentation"
      draggable
      onDragStart={(e) => {
        setDragging(tab.key);
        e.dataTransfer.effectAllowed = "move";
        e.dataTransfer.setData("text/plain", tab.key);
      }}
      onDragOver={(e) => dragging && dragging !== tab.key && e.preventDefault()}
      onDrop={(e) => {
        e.preventDefault();
        if (dragging) tabs.move(dragging, tab.key);
        setDragging(undefined);
      }}
      onDragEnd={() => setDragging(undefined)}
      className={cn("group relative flex shrink-0 items-center border-r bg-panel text-[12.5px]", active ? "bg-background text-foreground shadow-[inset_0_2px_0_var(--primary)]" : "text-muted-foreground hover:text-foreground", dragging === tab.key && "opacity-50")}
    >
      <button
        ref={ref}
        role="tab"
        id={`tab-${tab.key}`}
        aria-selected={active}
        tabIndex={active ? 0 : -1}
        title={label}
        onClick={() => tabs.open(tab.href)}
        onAuxClick={(e) => {
          if (e.button === 1) {
            e.preventDefault();
            tabs.close(tab.key);
          }
        }}
        onMouseDown={(e) => e.button === 1 && e.preventDefault()}
        className="flex max-w-[220px] items-center gap-2 py-2 pl-3.5 pr-1.5 outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring"
      >
        {status ? <StatusDot status={status} /> : null}
        <span className="truncate">{label}</span>
      </button>
      <button
        type="button"
        aria-label={`Close ${label}`}
        title="Close (Ctrl+W)"
        onClick={() => tabs.close(tab.key)}
        className={cn("mr-1.5 flex size-5 items-center justify-center rounded outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring", active ? "opacity-100" : "opacity-60 group-hover:opacity-100")}
      >
        <X className="size-3" />
      </button>
    </div>
  );
}

export function EditorTabs({ tabs }: { tabs: Tabs }) {
  const [dragging, setDragging] = useState<string>();
  const multi = new Set(tabs.tabs.map((t) => t.projectId).filter(Boolean)).size > 1;
  const onKey = (e: React.KeyboardEvent) => {
    if (e.key !== "ArrowRight" && e.key !== "ArrowLeft") return;
    e.preventDefault();
    tabs.next(e.key === "ArrowRight" ? 1 : -1);
  };
  return (
    <div role="tablist" aria-label="Open editors" onKeyDown={onKey} className="flex overflow-x-auto overflow-y-hidden bg-panel [scrollbar-width:thin]">
      {tabs.tabs.map((t) => (
        <TabButton key={t.key} tab={t} active={tabs.active?.key === t.key} multi={multi} tabs={tabs} dragging={dragging} setDragging={setDragging} />
      ))}
    </div>
  );
}
