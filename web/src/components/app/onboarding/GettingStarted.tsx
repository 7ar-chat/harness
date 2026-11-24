// The getting-started checklist: seven steps that tick themselves from real data, each one a link to the right place.
// It sits at the top of the Dashboard and, folded, in the explorer, until every step is done or it is dismissed.
import { useQuery } from "@tanstack/react-query";
import { Check, ChevronDown, ChevronRight, X } from "lucide-react";
import { api } from "@/api/client";
import { fk } from "@/api/factory";
import { useMembers, useOverview } from "@/api/queries";
import { useUsage } from "@/api/schedulesUsage";
import type { FactoryTask } from "@/api/factory";
import { useDialogs } from "@/components/app/dialogs";
import { useGithubConnector } from "@/components/app/github-import";
import { NavLink } from "@/components/app/shell/context";
import { projectHref } from "@/components/app/shell/tabs";
import { helpHref } from "@/lib/help";
import { requestOnboarding, useOnboarding } from "@/lib/onboarding";
import { cn } from "@/lib/utils";

export interface Step {
  id: string;
  label: string;
  hint: string;
  done: boolean;
  /** a place to go, or something to open */
  href?: string;
  onGo?: () => void;
}

/** The steps and whether each is done: from the overview, the project's tasks, members and spend cap, and what the person marked. */
export function useChecklist(projectId?: string): { steps: Step[]; done: number } {
  const overview = useOverview().data;
  const { state } = useOnboarding();
  const { newProject, newSession } = useDialogs();
  const gh = useGithubConnector();
  const pid = projectId ?? overview?.projects[0]?.id ?? "";
  const tasks = useQuery({ queryKey: fk.tasks(pid), queryFn: () => api.get<FactoryTask[]>(`/projects/${pid}/tasks?limit=500`), enabled: !!pid, retry: false });
  const members = useMembers(pid);
  const usage = useUsage({ projectId: pid || undefined, range: "month", group: "day" });
  const projects = overview?.projects ?? [];
  const sessions = [...(overview?.personal ?? []), ...projects.flatMap((p) => p.threads)].filter((t) => t.kind !== "supervisor");
  const hasProject = projects.length > 0;
  const needProject = (go: () => void) => (hasProject ? go : newProject);
  const steps: Step[] = [
    { id: "github", label: "Connect GitHub or add a repository", hint: "So agents can work on your real code.", done: gh.ready || projects.some((p) => !!p.repoUrl), href: "/connectors" },
    { id: "project", label: "Create a project", hint: "A repository your agents work in.", done: hasProject, onGo: newProject },
    { id: "session", label: "Start your first session", hint: "Chat with an agent and watch it work.", done: sessions.length > 0, onGo: () => newSession(pid || undefined) },
    { id: "approve", label: "Approve your first action", hint: "Agents ask before risky steps. Answer one.", done: !!state.marked?.approved, href: helpHref("approvals") },
    { id: "task", label: "Hand off your first task", hint: "Let an agent work on its own and open a pull request.", done: (tasks.data?.length ?? 0) > 0, href: pid ? projectHref(pid, "tasks") : undefined, onGo: needProject(() => undefined) },
    { id: "member", label: "Invite a teammate", hint: "Share the project with the people you work with.", done: (members.data?.length ?? 0) > 1, href: pid ? projectHref(pid, "members") : undefined, onGo: needProject(() => undefined) },
    { id: "cap", label: "Set a spend cap", hint: "A monthly limit, so cost never surprises you.", done: (usage.data?.cap?.monthlyUsd ?? 0) > 0, href: pid ? projectHref(pid, "usage") : "/usage" },
  ];
  return { steps, done: steps.filter((s) => s.done).length };
}

function StepRow({ s, compact, onNavigate }: { s: Step; compact?: boolean; onNavigate?: () => void }) {
  const box = (
    <span aria-hidden className={cn("flex size-4 shrink-0 items-center justify-center rounded-full border", s.done ? "border-success bg-success text-white" : "border-muted-foreground/50")}>
      {s.done ? <Check className="size-3" /> : null}
    </span>
  );
  const body = (
    <span className="min-w-0 flex-1">
      <span className={cn("block truncate", s.done && "text-muted-foreground line-through")}>{s.label}</span>
      {compact ? null : <span className="block text-xs text-muted-foreground">{s.hint}</span>}
    </span>
  );
  const cls = "flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-[13px] outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring";
  const state = <span className="sr-only">{s.done ? " (done)" : " (to do)"}</span>;
  if (s.href) {
    return (
      <NavLink href={s.href} onNavigate={onNavigate} className={cls}>
        {box}
        {body}
        {state}
      </NavLink>
    );
  }
  return (
    <button
      type="button"
      className={cls}
      onClick={() => {
        s.onGo?.();
        onNavigate?.();
      }}
    >
      {box}
      {body}
      {state}
    </button>
  );
}

export function GettingStarted({ variant, projectId, onNavigate }: { variant: "dashboard" | "explorer"; projectId?: string; onNavigate?: () => void }) {
  const { state, patch } = useOnboarding();
  const { steps, done } = useChecklist(projectId);
  if (state.dismissed || done === steps.length) return null;
  const pct = Math.round((done / steps.length) * 100);
  const title = "Getting started";
  if (variant === "explorer") {
    // Folded to a single progress line until the person opens it: the full checklist lives on the Dashboard.
    const open = state.folded === false;
    return (
      <section aria-label={`${title} summary`} className="mx-0.5 mb-1 mt-2 rounded-md border bg-card/60 text-card-foreground">
        <div className="flex items-center pr-1">
          <button type="button" aria-expanded={open} onClick={() => patch({ folded: open })} className="flex flex-1 items-center gap-1.5 rounded px-2 py-1.5 text-left text-xs font-medium outline-none focus-visible:ring-2 focus-visible:ring-ring">
            {open ? <ChevronDown className="size-3" aria-hidden /> : <ChevronRight className="size-3" aria-hidden />}
            {title}
            <span className="ml-auto font-normal text-muted-foreground">
              {done} of {steps.length}
            </span>
          </button>
          <button type="button" aria-label="Dismiss getting started" title="Dismiss" onClick={() => patch({ dismissed: true })} className="flex size-6 items-center justify-center rounded text-muted-foreground outline-none hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring">
            <X className="size-3.5" />
          </button>
        </div>
        <div className="mx-2 mb-1 h-1 overflow-hidden rounded-full bg-muted" role="progressbar" aria-label="Getting started progress" aria-valuenow={done} aria-valuemin={0} aria-valuemax={steps.length}>
          <div className="h-full bg-primary transition-all" style={{ width: `${pct}%` }} />
        </div>
        {open ? (
          <ul className="px-0.5 pb-1">
            {steps.map((s) => (
              <li key={s.id}>
                <StepRow s={s} compact onNavigate={onNavigate} />
              </li>
            ))}
          </ul>
        ) : null}
      </section>
    );
  }
  return (
    <section aria-label={title} className="rounded-lg border bg-card text-card-foreground">
      <div className="flex items-start gap-3 p-4 pb-2">
        <div className="min-w-0 flex-1 space-y-1">
          <h2 className="text-sm font-semibold">{title}</h2>
          <p className="text-xs text-muted-foreground">
            {done} of {steps.length} done. Each step takes you to the right place and ticks itself.
          </p>
        </div>
        <button type="button" onClick={() => requestOnboarding("tour")} className="rounded px-2 py-1 text-xs text-primary outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring">
          Take the tour
        </button>
        <button type="button" onClick={() => patch({ dismissed: true })} className="rounded px-2 py-1 text-xs text-muted-foreground outline-none hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring">
          Dismiss
        </button>
      </div>
      <div className="mx-4 mb-2 h-1.5 overflow-hidden rounded-full bg-muted" role="progressbar" aria-label="Getting started progress" aria-valuenow={done} aria-valuemin={0} aria-valuemax={steps.length}>
        <div className="h-full bg-primary transition-all" style={{ width: `${pct}%` }} />
      </div>
      <ul className="grid gap-x-2 px-2 pb-3 sm:grid-cols-2">
        {steps.map((s) => (
          <li key={s.id}>
            <StepRow s={s} />
          </li>
        ))}
      </ul>
    </section>
  );
}
