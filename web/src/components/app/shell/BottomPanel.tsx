// The bottom panel: what needs you (with the decisions inline), the live activity feed, running tasks and this month's spend.
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { formatDistanceToNowStrict } from "date-fns";
import { useMemo, useState, type ReactNode } from "react";
import { api } from "@/api/client";
import { useNeedsReview, useResumeTask, useTasks } from "@/api/factory";
import { qk, threadQuery, toastError, useOverview } from "@/api/queries";
import { useUsage } from "@/api/schedulesUsage";
import { useGateAction } from "@/api/workflows";
import type { InboxDoc, FeedEvent } from "@/api/types";
import { Button } from "@/components/ui/button";
import { useFeed } from "@/lib/feed";
import { formatUsd } from "@/lib/money";
import { cn } from "@/lib/utils";
import { StatusDot } from "../common";
import { NavLink, useShell, type PanelTab } from "./context";
import { projectHref } from "./tabs";

const usd = formatUsd;
const ago = (iso: string) => {
  try {
    return `${formatDistanceToNowStrict(new Date(iso))} ago`;
  } catch {
    return "";
  }
};

const Ab = ({ primary, ...p }: { primary?: boolean } & React.ComponentProps<typeof Button>) => (
  <Button size="xs" variant={primary ? "default" : "outline"} className="h-6 shrink-0 px-2.5 text-xs" {...p} />
);

interface NeedRow {
  key: string;
  kind: string;
  tone: string;
  title: string;
  detail?: string;
  actions: ReactNode;
}

function ThreadApprove({ id, projectId }: { id: string; projectId?: string }) {
  const q = useQuery({ ...threadQuery(id) });
  const qc = useQueryClient();
  const { tabs } = useShell();
  const [busy, setBusy] = useState(false);
  const pending = q.data?.pendingApproval;
  const decide = async (decision: "approve" | "deny") => {
    if (!pending) return;
    setBusy(true);
    try {
      await api.post(`/threads/${id}/resume`, { interruptId: pending.id, reason: "tool_approval", decision });
      void qc.invalidateQueries({ queryKey: qk.overview });
      void qc.invalidateQueries({ queryKey: qk.thread(id) });
      void qc.invalidateQueries({ queryKey: ["needs-review"] });
    } catch (e) {
      toastError(e);
    } finally {
      setBusy(false);
    }
  };
  void projectId;
  return (
    <>
      {pending ? (
        <>
          <Ab primary disabled={busy} onClick={() => void decide("approve")} aria-label={`Approve ${pending.toolName ?? "the tool call"}`}>
            Approve
          </Ab>
          <Ab disabled={busy} onClick={() => void decide("deny")} aria-label={`Deny ${pending.toolName ?? "the tool call"}`}>
            Deny
          </Ab>
        </>
      ) : null}
      <Ab onClick={() => tabs.open(`/t/${id}`)}>Review</Ab>
    </>
  );
}

function TaskActions({ pid, task, blocked }: { pid: string; task: InboxDoc["tasks"][number]["task"]; blocked: boolean }) {
  const resume = useResumeTask(pid);
  const { tabs } = useShell();
  const cap = task.budget?.maxUsd ?? 0;
  const raised = cap > 0 ? Math.ceil(cap * 1.6) : 0;
  const budgetBlocked = blocked && cap > 0 && task.spent.usd >= cap;
  return (
    <>
      {blocked && budgetBlocked ? (
        <Ab primary disabled={resume.isPending} onClick={() => resume.mutate({ tid: task.id, budget: { ...task.budget, maxUsd: raised } })}>
          Raise to {usd(raised)}
        </Ab>
      ) : null}
      {!blocked && task.pr?.url ? (
        <Ab primary asChild>
          <a href={task.pr.url} target="_blank" rel="noreferrer">
            Review
          </a>
        </Ab>
      ) : null}
      <Ab onClick={() => tabs.open(projectHref(pid, "tasks", { task: task.id }))}>{blocked ? "Open" : "Open task"}</Ab>
    </>
  );
}

function GateActions({ it }: { it: InboxDoc["gates"][number] }) {
  const act = useGateAction();
  const { tabs } = useShell();
  return (
    <>
      <Ab primary disabled={act.isPending} onClick={() => act.mutate({ runId: it.runId, nodeId: it.nodeId, action: "approve" })} aria-label={`Approve gate ${it.title}`}>
        Approve
      </Ab>
      <Ab onClick={() => tabs.open(projectHref(it.projectId, "workflows", { run: it.runId }))}>Open run</Ab>
    </>
  );
}

function NeedsYou() {
  const { projectId } = useShell();
  const q = useNeedsReview().data;
  const rows = useMemo<NeedRow[]>(() => {
    if (!q) return [];
    const mine = (pid?: string) => !projectId || pid === projectId;
    return [
      ...(q.threads ?? [])
        .filter((it) => mine(it.projectId))
        .map((it) => ({
          key: `s:${it.thread.id}`,
          kind: it.thread.status === "waiting_approval" ? "approval" : it.thread.status === "failed" ? "failed" : "question",
          tone: it.thread.status === "failed" ? "text-destructive" : "text-warning",
          title: it.thread.title || "(untitled)",
          detail: it.thread.pendingTool ?? it.thread.pendingQuestion,
          actions: it.thread.status === "waiting_approval" ? <ThreadApprove id={it.thread.id} projectId={it.projectId} /> : <ThreadReview id={it.thread.id} />,
        })),
      ...(q.gates ?? [])
        .filter((it) => mine(it.projectId))
        .map((it) => ({ key: `g:${it.runId}:${it.nodeId}`, kind: "gate", tone: "text-primary", title: `${it.workflowName}: ${it.title}`, detail: it.projectName, actions: <GateActions it={it} /> })),
      ...(q.tasks ?? [])
        .filter((it) => mine(it.projectId))
        .map((it) => ({
          key: `t:${it.task.id}`,
          kind: it.kind === "blocked" ? "blocked" : "merge",
          tone: it.kind === "blocked" ? "text-destructive" : "text-primary",
          title: it.task.title,
          detail: it.kind === "blocked" ? it.reason : it.task.pr?.number ? `PR #${it.task.pr.number} in review` : undefined,
          actions: <TaskActions pid={it.projectId} task={it.task} blocked={it.kind === "blocked"} />,
        })),
    ];
  }, [q, projectId]);
  if (!q) return <p className="p-3 text-muted-foreground">Loading…</p>;
  if (!rows.length) return <p className="p-3 text-muted-foreground">Nothing waits here.</p>;
  return (
    <ul aria-label="Waiting for you" className="flex flex-col gap-1.5 p-2.5">
      {rows.map((r) => (
        <li key={r.key} className="flex items-center gap-3">
          <span className={cn("w-[70px] shrink-0 text-xs font-medium", r.tone)}>{r.kind}</span>
          <span className="min-w-0 flex-1 truncate">
            {r.title}
            {r.detail ? <span className="text-muted-foreground"> · {r.detail}</span> : null}
          </span>
          <span className="flex shrink-0 items-center gap-1.5">{r.actions}</span>
        </li>
      ))}
    </ul>
  );
}

function ThreadReview({ id }: { id: string }) {
  const { tabs } = useShell();
  return <Ab onClick={() => tabs.open(`/t/${id}`)}>Review</Ab>;
}

const EVENT_TEXT: Partial<Record<FeedEvent["type"], string>> = {
  "run.started": "started a run",
  "run.finished": "finished a run",
  "message.sent": "sent a message",
  "thread.created": "created a session",
  "thread.updated": "updated a session",
  "thread.deleted": "deleted a session",
  "task.created": "created a task",
  "task.updated": "updated a task",
  "task.blocked": "blocked a task",
  "workflow.run": "workflow run changed",
  "workflow.gate": "a workflow gate changed",
  "workflow.changed": "workflow changed",
  "requirement.changed": "requirement changed",
  "memory.changed": "memory changed",
  "member.changed": "members changed",
  "project.changed": "project changed",
};

function Activity() {
  const { events } = useFeed();
  const { projectId } = useShell();
  const overview = useOverview().data;
  const shown = events.filter((e) => !projectId || !e.projectId || e.projectId === projectId);
  if (!shown.length) return <p className="p-3 text-muted-foreground">Live events from your projects show up here as they happen.</p>;
  const title = (e: FeedEvent) => e.title || [...(overview?.personal ?? []), ...(overview?.projects ?? []).flatMap((p) => p.threads)].find((t) => t.id === e.threadId)?.title || "";
  return (
    <ul aria-label="Activity" className="flex flex-col gap-0.5 p-2.5 font-mono text-xs">
      {shown.slice(0, 40).map((e, i) => (
        <li key={`${e.at}-${i}`} className="flex gap-3">
          <time dateTime={e.at} className="w-[72px] shrink-0 text-muted-foreground">
            {new Date(e.at).toLocaleTimeString([], { hour12: false })}
          </time>
          <span className="min-w-0 flex-1 truncate">
            {e.actor ? <span className="text-muted-foreground">{e.actor} </span> : null}
            {EVENT_TEXT[e.type] ?? e.type}
            {e.status ? ` (${e.status})` : ""}
            {title(e) ? (
              e.threadId ? (
                <>
                  {" · "}
                  <NavLink href={`/t/${e.threadId}`} className="text-primary hover:underline">
                    {title(e)}
                  </NavLink>
                </>
              ) : (
                ` · ${title(e)}`
              )
            ) : null}
          </span>
        </li>
      ))}
    </ul>
  );
}

function TasksNow() {
  const { projectId } = useShell();
  const tasks = useTasks(projectId ?? "");
  const { tabs } = useShell();
  if (!projectId) return <p className="p-3 text-muted-foreground">Pick a project to see its tasks.</p>;
  const live = (tasks.data ?? []).filter((t) => t.state === "running" || t.state === "blocked" || t.state === "review");
  if (tasks.isLoading) return <p className="p-3 text-muted-foreground">Loading…</p>;
  if (!live.length) return <p className="p-3 text-muted-foreground">No task is running or blocked.</p>;
  return (
    <ul aria-label="Running tasks" className="flex flex-col gap-1 p-2.5">
      {live.map((t) => (
        <li key={t.id} className="flex items-center gap-3">
          <StatusDot status={t.state === "running" ? "running" : t.state === "blocked" ? "failed" : "waiting_approval"} />
          <span className="w-16 shrink-0 text-xs text-muted-foreground">{t.state}</span>
          <button type="button" className="min-w-0 flex-1 truncate text-left hover:underline" onClick={() => tabs.open(projectHref(projectId, "tasks", { task: t.id }))}>
            {t.title}
          </button>
          <span className="shrink-0 font-mono text-xs text-muted-foreground">
            {usd(t.spent.usd)}
            {t.budget?.maxUsd ? ` / ${usd(t.budget.maxUsd)}` : ""}
          </span>
          <span className="w-24 shrink-0 text-right text-xs text-muted-foreground">{ago(t.updatedAt)}</span>
        </li>
      ))}
    </ul>
  );
}

function UsageNow() {
  const { projectId } = useShell();
  const u = useUsage({ projectId, range: "month", group: "day" }).data;
  if (!u) return <p className="p-3 text-muted-foreground">No usage data.</p>;
  const cap = u.cap?.monthlyUsd ?? 0;
  const ratio = cap > 0 ? Math.min(1, u.total.costUsd / cap) : 0;
  return (
    <div className="space-y-2 p-3">
      <div className="flex items-baseline gap-3">
        <span className="font-mono text-lg">{usd(u.total.costUsd)}</span>
        <span className="text-muted-foreground">this month{cap ? ` of ${usd(cap)}` : ", no cap set"}</span>
        <span className="ml-auto text-xs text-muted-foreground">{u.total.runs} runs</span>
      </div>
      {cap > 0 ? (
        <div role="progressbar" aria-label="Spend against the monthly cap" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(ratio * 100)} className="h-2 overflow-hidden rounded-full bg-muted">
          <div className={cn("h-full rounded-full", u.cap.reached ? "bg-destructive" : u.cap.warn ? "bg-warning" : "bg-primary")} style={{ width: `${ratio * 100}%` }} />
        </div>
      ) : null}
    </div>
  );
}

export function BottomPanel({ onClose, handle }: { onClose?: () => void; handle?: ReactNode }) {
  const { panelTab, setPanelTab, projectId } = useShell();
  const review = useNeedsReview().data;
  const n = review
    ? [...(review.threads ?? []).filter((x) => !projectId || x.projectId === projectId), ...(review.gates ?? []).filter((x) => !projectId || x.projectId === projectId), ...(review.tasks ?? []).filter((x) => !projectId || x.projectId === projectId)].length
    : 0;
  const tabs: [PanelTab, string][] = [
    ["needs", `Needs you · ${n}`],
    ["activity", "Activity"],
    ["tasks", "Tasks"],
    ["usage", "Usage"],
  ];
  return (
    <section aria-label="Panel" className="flex h-full min-h-0 flex-col bg-panel text-[13px]">
      {handle}
      <div className="flex shrink-0 items-center border-b">
        <div role="tablist" aria-label="Panel views" className="flex">
          {tabs.map(([id, label]) => (
            <button
              key={id}
              role="tab"
              id={`panel-tab-${id}`}
              aria-selected={panelTab === id}
              aria-controls="panel-body"
              onClick={() => setPanelTab(id)}
              className={cn("border-b-2 px-3 py-1.5 text-xs outline-none focus-visible:ring-2 focus-visible:ring-ring", panelTab === id ? "border-primary text-foreground" : "border-transparent text-muted-foreground hover:text-foreground")}
            >
              {label}
            </button>
          ))}
        </div>
        {onClose ? (
          <button type="button" onClick={onClose} aria-label="Close panel" className="ml-auto mr-1 rounded px-2 py-1 text-xs text-muted-foreground hover:text-foreground">
            Close
          </button>
        ) : null}
      </div>
      <div id="panel-body" role="tabpanel" aria-labelledby={`panel-tab-${panelTab}`} className="min-h-0 flex-1 overflow-y-auto">
        {panelTab === "needs" ? <NeedsYou /> : panelTab === "activity" ? <Activity /> : panelTab === "tasks" ? <TasksNow /> : <UsageNow />}
      </div>
    </section>
  );
}
