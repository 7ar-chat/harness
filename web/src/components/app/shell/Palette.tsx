// Ctrl/Cmd+K: jump to anything and run commands. Ctrl/Cmd+P: switch project.
import { useMemo } from "react";
import { useOverview } from "@/api/queries";
import { CommandDialog, CommandEmpty, CommandGroup, CommandInput, CommandItem, CommandList, CommandShortcut } from "@/components/ui/command";
import { requestOnboarding } from "@/lib/onboarding";
import { useTheme } from "@/lib/theme";
import { StatusDot } from "../common";
import { useDialogs } from "../dialogs";
import { useShell } from "./context";
import { useProjectActions } from "./projectActions";
import { useTabLabel } from "./EditorTabs";
import { PROJECT_SECTIONS, projectHref, type EditorTab } from "./tabs";

const PAGES: [string, string][] = [
  ["/", "Dashboard"],
  ["/inbox", "Inbox"],
  ["/supervisor", "My assistant"],
  ["/jobs", "Jobs"],
  ["/references", "References"],
  ["/connectors", "Connectors"],
  ["/memory", "Library (my memory)"],
  ["/schedules", "My schedules"],
  ["/roles", "My roles (personal)"],
  ["/usage", "My usage"],
  ["/insights", "My insights"],
  ["/capabilities", "My tools & skills"],
  ["/help", "Help"],
  ["/profile", "Profile and settings"],
];

function OpenTab({ tab, onPick }: { tab: EditorTab; onPick: () => void }) {
  const label = useTabLabel(tab, true);
  return (
    <CommandItem value={`tab ${label} ${tab.key}`} onSelect={onPick}>
      {label}
      <span className="ml-auto text-xs text-muted-foreground">open tab</span>
    </CommandItem>
  );
}

export function Palette({ open, setOpen }: { open: boolean; setOpen: (o: boolean) => void }) {
  const overview = useOverview().data;
  const { tabs, projectId, setExplorer, setPanel } = useShell();
  const { newSession, newProject } = useDialogs();
  const { setTheme, resolvedTheme } = useTheme();
  const go = (fn: () => void) => () => {
    setOpen(false);
    fn();
  };
  const project = overview?.projects.find((p) => p.id === projectId);
  const pa = useProjectActions(projectId);
  const sessions = useMemo(() => [...(overview?.personal ?? []).map((t) => ({ t, project: "Personal" })), ...(overview?.projects ?? []).flatMap((p) => p.threads.map((t) => ({ t, project: p.name })))], [overview]);
  return (
    <CommandDialog open={open} onOpenChange={setOpen} title="Search" description="Jump to a session, tab or project, or run a command">
      <CommandInput placeholder="Jump to a session, tab, project or command…" />
      <CommandList>
        <CommandEmpty>Nothing found.</CommandEmpty>
        {tabs.tabs.length > 1 ? (
          <CommandGroup heading="Open tabs">
            {tabs.tabs.map((t) => (
              <OpenTab key={t.key} tab={t} onPick={go(() => tabs.open(t.href))} />
            ))}
          </CommandGroup>
        ) : null}
        <CommandGroup heading="Go to">
          {PAGES.map(([to, label]) => (
            <CommandItem key={to} value={`go ${label}`} onSelect={go(() => tabs.open(to))}>
              {label}
            </CommandItem>
          ))}
        </CommandGroup>
        {project ? (
          <CommandGroup heading={project.name}>
            {Object.entries(PROJECT_SECTIONS).map(([s, label]) => (
              <CommandItem key={s} value={`${project.name} ${label}`} onSelect={go(() => tabs.open(projectHref(project.id, s)))}>
                {label}
              </CommandItem>
            ))}
          </CommandGroup>
        ) : null}
        <CommandGroup heading="Try">
          <CommandItem value="try start a session and ask an agent" onSelect={go(() => newSession(project?.id))}>
            Try: start a session and ask an agent
          </CommandItem>
          <CommandItem value="try create a project from a template" onSelect={go(newProject)}>
            Try: create a project from a template
          </CommandItem>
          <CommandItem value="try take the 1 minute tour" onSelect={go(() => requestOnboarding("tour"))}>
            Try: take the one-minute tour
          </CommandItem>
        </CommandGroup>
        <CommandGroup heading="Actions">
          <CommandItem onSelect={go(() => newSession(project?.id))}>New session</CommandItem>
          <CommandItem onSelect={go(newProject)}>New project</CommandItem>
          {pa?.canWrite ? <CommandItem onSelect={go(pa.importAgents)} disabled={!pa.canImport}>Import AGENTS.md into the project memory</CommandItem> : null}
          {pa ? <CommandItem onSelect={go(pa.exportMemory)}>Export the project's MEMORY.md</CommandItem> : null}
          <CommandItem value="show keyboard shortcuts help" onSelect={go(() => requestOnboarding("shortcuts"))}>
            Show keyboard shortcuts <CommandShortcut>?</CommandShortcut>
          </CommandItem>
          <CommandItem value="open help center articles" onSelect={go(() => tabs.open("/help"))}>
            Open help
          </CommandItem>
          <CommandItem value="getting started welcome replay" onSelect={go(() => requestOnboarding("welcome"))}>
            Getting started
          </CommandItem>
          <CommandItem onSelect={go(() => setTheme(resolvedTheme === "dark" ? "light" : "dark"))}>Toggle theme</CommandItem>
          <CommandItem onSelect={go(() => setExplorer((v) => !v))}>
            Toggle explorer <CommandShortcut>⌘B</CommandShortcut>
          </CommandItem>
          <CommandItem onSelect={go(() => setPanel((v) => !v))}>
            Toggle panel <CommandShortcut>⌘J</CommandShortcut>
          </CommandItem>
          {tabs.active ? (
            <CommandItem onSelect={go(() => tabs.close(tabs.active!.key))}>
              Close tab <CommandShortcut>Ctrl W</CommandShortcut>
            </CommandItem>
          ) : null}
        </CommandGroup>
        {overview?.projects.length ? (
          <CommandGroup heading="Projects">
            {overview.projects.map((p) => (
              <CommandItem key={p.id} value={`project ${p.name}`} onSelect={go(() => tabs.open(projectHref(p.id)))}>
                {p.name}
              </CommandItem>
            ))}
          </CommandGroup>
        ) : null}
        <CommandGroup heading="Sessions">
          {sessions
            .filter(({ t }) => t.kind !== "supervisor")
            .map(({ t, project }) => (
              <CommandItem key={t.id} value={`${t.title} ${project} ${t.id}`} onSelect={go(() => tabs.open(`/t/${t.id}`))}>
                <StatusDot status={t.status} />
                {t.title || "(untitled)"}
                <span className="ml-auto text-xs text-muted-foreground">{project}</span>
              </CommandItem>
            ))}
        </CommandGroup>
      </CommandList>
    </CommandDialog>
  );
}

export function ProjectSwitcher({ open, setOpen }: { open: boolean; setOpen: (o: boolean) => void }) {
  const overview = useOverview().data;
  const { tabs, projectId, setProjectId } = useShell();
  const pick = (id?: string) => () => {
    setOpen(false);
    setProjectId(id);
    tabs.open(id ? projectHref(id) : "/");
  };
  return (
    <CommandDialog open={open} onOpenChange={setOpen} title="Switch project" description="Choose the project the explorer shows">
      <CommandInput placeholder="Switch project…" />
      <CommandList>
        <CommandEmpty>No such project.</CommandEmpty>
        <CommandGroup heading="Projects">
          {(overview?.projects ?? []).map((p) => (
            <CommandItem key={p.id} value={`project ${p.name}`} onSelect={pick(p.id)}>
              {p.name}
              {p.id === projectId ? <span className="ml-auto text-xs text-muted-foreground">current</span> : null}
            </CommandItem>
          ))}
          <CommandItem value="personal space" onSelect={pick(undefined)}>
            Personal space
          </CommandItem>
        </CommandGroup>
      </CommandList>
    </CommandDialog>
  );
}
