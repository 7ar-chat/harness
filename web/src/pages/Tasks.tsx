// The "Tasks" tab of a project: the agent factory's tasks, triggers and spend report.
import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import { formatDistanceToNowStrict } from "date-fns";
import { ExternalLink, GitPullRequest, Plus, Trash2 } from "lucide-react";
import { useState } from "react";
import { isUnsupported } from "@/api/client";
import {
  TASK_STATE_LABEL, isTerminalState, useCancelTask, useCreateTask, useCreateTrigger, useDeleteTrigger, useFactoryReport, useLearning, useResumeTask, useTask, useTasks, useTriggers,
  type FactoryState, type FactoryTask, type FactoryTaskEvent, type LearningCluster, type LearningExperiment, type TaskDetail, type TriggerInput,
} from "@/api/factory";
import { useRoles } from "@/api/roles";
import type { Role } from "@/api/types";
import { Empty, ErrorState, ListSkeleton } from "@/components/app/common";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";

const ago = (iso: string) => `${formatDistanceToNowStrict(new Date(iso))} ago`;
const usd = (n: number) => `$${n.toFixed(n >= 100 ? 0 : 2)}`;
const num = (s: string) => {
  const n = Number(s);
  return s.trim() !== "" && Number.isFinite(n) && n > 0 ? n : undefined;
};

const STATE_TONE: Record<FactoryState, string> = {
  pending: "bg-muted text-muted-foreground",
  ready: "bg-muted text-foreground",
  running: "bg-primary/15 text-primary",
  review: "bg-primary/15 text-primary",
  blocked: "bg-warning text-warning-foreground",
  done: "bg-success/15 text-success",
  failed: "bg-destructive/15 text-destructive",
  cancelled: "bg-muted text-muted-foreground",
};

export function TaskStateBadge({ state }: { state: FactoryState }) {
  return <span className={`inline-flex shrink-0 items-center rounded-full px-2 py-0.5 text-xs font-medium ${STATE_TONE[state]}`}>{TASK_STATE_LABEL[state]}</span>;
}

const FILTERS: { id: string; label: string; match: (s: FactoryState) => boolean }[] = [
  { id: "all", label: "All", match: () => true },
  { id: "active", label: "Active", match: (s) => s === "pending" || s === "ready" || s === "running" },
  { id: "blocked", label: "Blocked", match: (s) => s === "blocked" },
  { id: "review", label: "In review", match: (s) => s === "review" },
  { id: "done", label: "Done", match: (s) => s === "done" },
  { id: "failed", label: "Failed", match: (s) => s === "failed" || s === "cancelled" },
];

/** Spend against the cap, in dollars and tokens. */
function spendLine(t: FactoryTask) {
  const parts = [t.budget?.maxUsd ? `${usd(t.spent.usd)} of ${usd(t.budget.maxUsd)}` : usd(t.spent.usd)];
  parts.push(t.budget?.maxTokens ? `${t.spent.tokens.toLocaleString()} of ${t.budget.maxTokens.toLocaleString()} tokens` : `${t.spent.tokens.toLocaleString()} tokens`);
  return parts.join(" · ");
}

/** What the newest CI and review events say. */
function latest(events: FactoryTaskEvent[], kind: string, key: string) {
  const e = [...events].reverse().find((x) => x.kind === kind);
  const v = e?.data?.[key];
  return typeof v === "string" ? v : undefined;
}

/** Resume a blocked task with a word for the agent and, optionally, a higher spend cap. */
export function ResumeForm({ projectId, task, onDone }: { projectId: string; task: FactoryTask; onDone?: () => void }) {
  const resume = useResumeTask(projectId);
  const [feedback, setFeedback] = useState("");
  const [maxUsd, setMaxUsd] = useState("");
  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    const usdCap = num(maxUsd);
    resume.mutate({ tid: task.id, feedback: feedback.trim() || undefined, budget: usdCap ? { ...task.budget, maxUsd: usdCap } : undefined }, { onSuccess: () => { setFeedback(""); setMaxUsd(""); onDone?.(); } });
  };
  return (
    <form onSubmit={submit} className="space-y-2">
      <Textarea value={feedback} onChange={(e) => setFeedback(e.target.value)} placeholder="Tell the agent what to do next (optional)" aria-label="Feedback" rows={2} />
      <div className="flex flex-wrap items-center gap-2">
        <Input type="number" min="0" step="0.5" value={maxUsd} onChange={(e) => setMaxUsd(e.target.value)} placeholder={task.budget?.maxUsd ? `Raise cap from ${usd(task.budget.maxUsd)}` : "Set a USD cap"} aria-label="Budget in USD" className="h-8 w-48" />
        <Button type="submit" size="sm" disabled={resume.isPending}>
          Resume
        </Button>
      </div>
    </form>
  );
}

function Timeline({ events }: { events: FactoryTaskEvent[] }) {
  if (!events.length) return <p className="text-sm text-muted-foreground">No history yet.</p>;
  return (
    <ol className="space-y-2 border-l pl-4">
      {[...events].reverse().map((e) => (
        <li key={e.id} className="relative text-sm">
          <span className="absolute -left-[21px] top-1.5 size-2 rounded-full bg-border" />
          <div className="flex items-center gap-2">
            <Badge variant="outline" className="text-[10px]">
              {e.kind}
            </Badge>
            <time className="text-xs text-muted-foreground" dateTime={e.at}>
              {ago(e.at)}
            </time>
          </div>
          {e.text ? <p className="mt-0.5 whitespace-pre-wrap break-words text-muted-foreground">{e.text}</p> : null}
        </li>
      ))}
    </ol>
  );
}

function Fact({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className="text-sm">{children}</dd>
    </div>
  );
}

function TaskBody({ projectId, detail, canWrite }: { projectId: string; detail: TaskDetail; canWrite: boolean }) {
  const { task: t, events } = detail;
  const cancel = useCancelTask(projectId);
  const [confirm, setConfirm] = useState(false);
  const ci = latest(events, "ci", "state");
  const review = latest(events, "review", "verdict");
  return (
    <div className="space-y-5 overflow-y-auto px-4 pb-6">
      <div className="flex items-center gap-2">
        <TaskStateBadge state={t.state} />
        {t.role ? <Badge variant="outline">{t.role}</Badge> : null}
        {t.policy ? <Badge variant="outline">{t.policy}</Badge> : null}
      </div>
      {t.blockedReason ? <div className="rounded-md bg-warning/15 p-3 text-sm">{t.blockedReason}</div> : null}
      {t.error ? <div className="rounded-md bg-destructive/10 p-3 text-sm text-destructive">{t.error}</div> : null}
      <dl className="grid grid-cols-2 gap-3">
        <Fact label="Attempts">
          {t.attempts}
          {t.maxAttempts ? ` of ${t.maxAttempts}` : ""}
        </Fact>
        <Fact label="Spend">{spendLine(t)}</Fact>
        {t.spent.unpriced ? <Fact label="Unpriced runs">{t.spent.unpriced} (a model without a known price: its cost shows as $0)</Fact> : null}
        <Fact label="Pull request">
          {t.pr?.number ? (
            <a href={t.pr.url} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 underline">
              <GitPullRequest className="size-3.5" /> #{t.pr.number} <ExternalLink className="size-3" />
            </a>
          ) : (
            "None yet"
          )}
        </Fact>
        <Fact label="CI">{ci ?? "No result yet"}</Fact>
        <Fact label="Review">{review ?? "No verdict yet"}</Fact>
        <Fact label="Session">
          {t.threadId ? (
            <Link to="/t/$threadId" params={{ threadId: t.threadId }} className="underline">
              Open the agent's session
            </Link>
          ) : (
            "Not started"
          )}
        </Fact>
        {t.model ? <Fact label="Model">{t.model}</Fact> : null}
        {t.autoMerge ? <Fact label="Merge">Automatic once CI and review pass</Fact> : null}
        {t.dependsOn?.length ? <Fact label="Depends on">{t.dependsOn.join(", ")}</Fact> : null}
      </dl>
      <section>
        <h3 className="mb-1 text-sm font-medium">Spec</h3>
        <p className="whitespace-pre-wrap break-words text-sm text-muted-foreground">{t.spec}</p>
      </section>
      {t.result ? (
        <section>
          <h3 className="mb-1 text-sm font-medium">Result</h3>
          <p className="whitespace-pre-wrap break-words text-sm text-muted-foreground">{t.result}</p>
        </section>
      ) : null}
      {canWrite && t.state === "blocked" ? (
        <section>
          <h3 className="mb-2 text-sm font-medium">Resume</h3>
          <ResumeForm projectId={projectId} task={t} />
        </section>
      ) : null}
      {canWrite && !isTerminalState(t.state) ? (
        <Button variant="outline" size="sm" onClick={() => setConfirm(true)}>
          Cancel task
        </Button>
      ) : null}
      <section>
        <h3 className="mb-2 text-sm font-medium">History</h3>
        <Timeline events={events} />
      </section>
      <AlertDialog open={confirm} onOpenChange={setConfirm}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Cancel “{t.title}”?</AlertDialogTitle>
            <AlertDialogDescription>The agent stops working on it. This cannot be undone.</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Keep it</AlertDialogCancel>
            <AlertDialogAction onClick={() => cancel.mutate(t.id)}>Cancel task</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function TaskDrawer({ projectId, taskId, canWrite, onClose }: { projectId: string; taskId?: string; canWrite: boolean; onClose: () => void }) {
  const q = useTask(projectId, taskId);
  return (
    <Sheet open={!!taskId} onOpenChange={(o) => !o && onClose()}>
      <SheetContent className="w-full gap-3 sm:max-w-xl">
        <SheetHeader>
          <SheetTitle>{q.data?.task.title ?? "Task"}</SheetTitle>
          <SheetDescription>{q.data ? `Created ${ago(q.data.task.createdAt)} by ${q.data.task.owner}` : "Task details"}</SheetDescription>
        </SheetHeader>
        {q.isLoading ? <ListSkeleton rows={3} /> : q.error || !q.data ? <ErrorState error={q.error} retry={() => void q.refetch()} /> : <TaskBody projectId={projectId} detail={q.data} canWrite={canWrite} />}
      </SheetContent>
    </Sheet>
  );
}

const ROLES = ["coder", "planner", "reviewer", "tester", "explorer"];
const POLICIES = [
  ["default", "Server default"],
  ["supervised", "Supervised: every risky call asks"],
  ["sandboxed", "Sandboxed: push and merge ask"],
  ["autonomous", "Autonomous: only protected rules deny"],
] as const;

function CreateTaskDialog({ projectId, owner, open, onOpenChange, existing }: { projectId: string; owner: boolean; open: boolean; onOpenChange: (o: boolean) => void; existing: FactoryTask[] }) {
  const create = useCreateTask(projectId);
  // The project's own roles are offered too; one that names an external executor says so (the task then runs on that CLI).
  const projectRoles = (useRoles(projectId).data ?? []).filter((r) => r.enabled && !ROLES.includes(r.name));
  const [f, setF] = useState({ title: "", spec: "", role: "coder", model: "", policy: "default", maxUsd: "", maxAttempts: "", autoMerge: false });
  const [deps, setDeps] = useState<string[]>([]);
  const set = <K extends keyof typeof f>(k: K, v: (typeof f)[K]) => setF((x) => ({ ...x, [k]: v }));
  const open_ = existing.filter((t) => !isTerminalState(t.state));
  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    const maxUsd = num(f.maxUsd);
    create.mutate(
      {
        title: f.title.trim(),
        spec: f.spec.trim(),
        role: f.role === "coder" ? undefined : f.role,
        model: f.model.trim() || undefined,
        policy: f.policy === "default" ? undefined : f.policy,
        budget: maxUsd ? { maxUsd } : undefined,
        maxAttempts: num(f.maxAttempts),
        autoMerge: owner && f.autoMerge ? true : undefined,
        dependsOn: deps.length ? deps : undefined,
      },
      {
        onSuccess: () => {
          setF({ title: "", spec: "", role: "coder", model: "", policy: "default", maxUsd: "", maxAttempts: "", autoMerge: false });
          setDeps([]);
          onOpenChange(false);
        },
      },
    );
  };
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-lg">
        <form onSubmit={submit} className="space-y-3">
          <DialogHeader>
            <DialogTitle>New task</DialogTitle>
            <DialogDescription>An agent works on it unattended and opens a pull request.</DialogDescription>
          </DialogHeader>
          <div className="space-y-1">
            <Label htmlFor="task-title">Title</Label>
            <Input id="task-title" required value={f.title} onChange={(e) => set("title", e.target.value)} />
          </div>
          <div className="space-y-1">
            <Label htmlFor="task-spec">Spec</Label>
            <Textarea id="task-spec" required rows={4} value={f.spec} onChange={(e) => set("spec", e.target.value)} placeholder="What to do, in the words the agent will read" />
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1">
              <Label>Role</Label>
              <Select value={f.role} onValueChange={(v) => set("role", v)}>
                <SelectTrigger aria-label="Role" className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {ROLES.map((r) => (
                    <SelectItem key={r} value={r}>
                      {r}
                    </SelectItem>
                  ))}
                  {projectRoles.map((r) => (
                    <SelectItem key={r.name} value={r.name}>
                      {r.name}
                      {r.executor ? ` (runs on ${r.executor})` : ""}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              {projectRoles.find((r) => r.name === f.role)?.executor ? (
                <p className="text-xs text-muted-foreground">
                  Runs on the external executor {projectRoles.find((r) => r.name === f.role)?.executor}, under the policy below. It uses your API key from Settings.
                </p>
              ) : null}
            </div>
            <div className="space-y-1">
              <Label htmlFor="task-model">Model</Label>
              <Input id="task-model" value={f.model} onChange={(e) => set("model", e.target.value)} placeholder="Server default" />
            </div>
          </div>
          <div className="space-y-1">
            <Label>Policy</Label>
            <Select value={f.policy} onValueChange={(v) => set("policy", v)}>
              <SelectTrigger aria-label="Policy" className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {POLICIES.map(([v, l]) => (
                  <SelectItem key={v} value={v}>
                    {l}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1">
              <Label htmlFor="task-budget">Budget (USD)</Label>
              <Input id="task-budget" type="number" min="0" step="0.5" value={f.maxUsd} onChange={(e) => set("maxUsd", e.target.value)} placeholder="No cap" />
            </div>
            <div className="space-y-1">
              <Label htmlFor="task-attempts">Max attempts</Label>
              <Input id="task-attempts" type="number" min="1" step="1" value={f.maxAttempts} onChange={(e) => set("maxAttempts", e.target.value)} placeholder="Server default" />
            </div>
          </div>
          {open_.length ? (
            <fieldset className="space-y-1">
              <legend className="text-sm font-medium">Depends on</legend>
              <div className="max-h-28 space-y-1 overflow-y-auto rounded-md border p-2">
                {open_.map((t) => (
                  <label key={t.id} className="flex items-center gap-2 text-sm">
                    <input type="checkbox" checked={deps.includes(t.id)} onChange={(e) => setDeps((d) => (e.target.checked ? [...d, t.id] : d.filter((x) => x !== t.id)))} />
                    <span className="truncate">{t.title}</span>
                  </label>
                ))}
              </div>
            </fieldset>
          ) : null}
          <div className="flex items-center justify-between gap-3 rounded-md border p-3">
            <div>
              <Label htmlFor="task-merge">Merge automatically</Label>
              <p className="text-xs text-muted-foreground">{owner ? "Merge once CI is green and the review approves." : "Only a project owner can allow this."}</p>
            </div>
            <Switch id="task-merge" disabled={!owner} checked={f.autoMerge} onCheckedChange={(v) => set("autoMerge", v)} />
          </div>
          <DialogFooter>
            <Button type="submit" disabled={create.isPending || !f.title.trim() || !f.spec.trim()}>
              Create task
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function TaskList({ projectId, role }: { projectId: string; role: Role }) {
  const tasks = useTasks(projectId);
  const [filter, setFilter] = useState("all");
  const navigate = useNavigate();
  // The open task is in the address (?task=), so the inbox and a shared link land on it.
  const openId = useSearch({ from: "/p/$projectId" }).task;
  const setOpenId = (task?: string) => void navigate({ to: "/p/$projectId", params: { projectId }, search: { tab: "tasks", task }, replace: true });
  const [creating, setCreating] = useState(false);
  const canWrite = role !== "viewer";
  if (tasks.isLoading) return <ListSkeleton />;
  if (tasks.error) {
    if (isUnsupported(tasks.error)) return <Empty title="The agent factory is not enabled on this server" />;
    return <ErrorState error={tasks.error} retry={() => void tasks.refetch()} />;
  }
  const all = tasks.data ?? [];
  const f = FILTERS.find((x) => x.id === filter) ?? FILTERS[0]!;
  const shown = all.filter((t) => f.match(t.state));
  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2">
        <div className="flex flex-wrap gap-1" role="group" aria-label="Filter by state">
          {FILTERS.map((x) => {
            const n = all.filter((t) => x.match(t.state)).length;
            return (
              <Button key={x.id} size="sm" variant={filter === x.id ? "secondary" : "ghost"} aria-pressed={filter === x.id} onClick={() => setFilter(x.id)}>
                {x.label} {n ? <span className="text-xs text-muted-foreground">{n}</span> : null}
              </Button>
            );
          })}
        </div>
        {canWrite ? (
          <Button size="sm" className="ml-auto" onClick={() => setCreating(true)}>
            <Plus /> New task
          </Button>
        ) : null}
      </div>
      {shown.length ? (
        <ul className="divide-y rounded-lg border">
          {shown.map((t) => (
            <li key={t.id}>
              <button type="button" onClick={() => setOpenId(t.id)} className="flex w-full items-center gap-3 p-3 text-left hover:bg-accent">
                <TaskStateBadge state={t.state} />
                <span className="min-w-0 flex-1 truncate text-sm">{t.title}</span>
                {t.pr?.number ? <span className="text-xs text-muted-foreground">PR #{t.pr.number}</span> : null}
                <span className="hidden text-xs text-muted-foreground sm:inline">{spendLine(t)}</span>
                <span className="text-xs text-muted-foreground">{ago(t.updatedAt)}</span>
              </button>
            </li>
          ))}
        </ul>
      ) : (
        <Empty title={all.length ? "No task in this state" : "No tasks yet"}>{all.length ? null : "Create one to put an agent to work unattended."}</Empty>
      )}
      <TaskDrawer projectId={projectId} taskId={openId} canWrite={canWrite} onClose={() => setOpenId(undefined)} />
      <CreateTaskDialog projectId={projectId} owner={role === "owner"} open={creating} onOpenChange={setCreating} existing={all} />
    </div>
  );
}

function CreateTriggerDialog({ projectId, open, onOpenChange }: { projectId: string; open: boolean; onOpenChange: (o: boolean) => void }) {
  const create = useCreateTrigger(projectId);
  const blank = { name: "", kind: "cron" as "cron" | "webhook", schedule: "", event: "", label: "", title: "", spec: "", enabled: true };
  const [f, setF] = useState(blank);
  const set = <K extends keyof typeof f>(k: K, v: (typeof f)[K]) => setF((x) => ({ ...x, [k]: v }));
  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    const body: TriggerInput = { name: f.name.trim(), kind: f.kind, enabled: f.enabled, template: { title: f.title.trim(), spec: f.spec.trim() } };
    if (f.kind === "cron") body.schedule = f.schedule.trim();
    else {
      body.event = f.event.trim();
      if (f.label.trim()) body.match = { label: f.label.trim() };
    }
    create.mutate(body, { onSuccess: () => { setF(blank); onOpenChange(false); } });
  };
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-lg">
        <form onSubmit={submit} className="space-y-3">
          <DialogHeader>
            <DialogTitle>New trigger</DialogTitle>
            <DialogDescription>Creates a task on a schedule or when GitHub sends an event. The title and spec are Go templates.</DialogDescription>
          </DialogHeader>
          <div className="space-y-1">
            <Label htmlFor="trg-name">Name</Label>
            <Input id="trg-name" required value={f.name} onChange={(e) => set("name", e.target.value)} />
          </div>
          <div className="space-y-1">
            <Label>Kind</Label>
            <Select value={f.kind} onValueChange={(v) => set("kind", v as "cron" | "webhook")}>
              <SelectTrigger aria-label="Kind" className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="cron">Schedule (cron)</SelectItem>
                <SelectItem value="webhook">GitHub webhook</SelectItem>
              </SelectContent>
            </Select>
          </div>
          {f.kind === "cron" ? (
            <div className="space-y-1">
              <Label htmlFor="trg-schedule">Schedule</Label>
              <Input id="trg-schedule" required value={f.schedule} onChange={(e) => set("schedule", e.target.value)} placeholder="0 2 * * *  or  @every 6h" />
            </div>
          ) : (
            <div className="grid grid-cols-2 gap-3">
              <div className="space-y-1">
                <Label htmlFor="trg-event">Event</Label>
                <Input id="trg-event" required value={f.event} onChange={(e) => set("event", e.target.value)} placeholder="issues.labeled" />
              </div>
              <div className="space-y-1">
                <Label htmlFor="trg-label">Only with label</Label>
                <Input id="trg-label" value={f.label} onChange={(e) => set("label", e.target.value)} placeholder="agent" />
              </div>
            </div>
          )}
          <div className="space-y-1">
            <Label htmlFor="trg-title">Task title</Label>
            <Input id="trg-title" required value={f.title} onChange={(e) => set("title", e.target.value)} placeholder="Nightly dependency check" />
          </div>
          <div className="space-y-1">
            <Label htmlFor="trg-spec">Task spec</Label>
            <Textarea id="trg-spec" rows={4} value={f.spec} onChange={(e) => set("spec", e.target.value)} />
          </div>
          <div className="flex items-center justify-between rounded-md border p-3">
            <Label htmlFor="trg-enabled">Enabled</Label>
            <Switch id="trg-enabled" checked={f.enabled} onCheckedChange={(v) => set("enabled", v)} />
          </div>
          <DialogFooter>
            <Button type="submit" disabled={create.isPending}>
              Create trigger
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function Triggers({ projectId, owner }: { projectId: string; owner: boolean }) {
  const q = useTriggers(projectId);
  const del = useDeleteTrigger(projectId);
  const [creating, setCreating] = useState(false);
  const [target, setTarget] = useState<{ id: string; name: string }>();
  if (q.isLoading) return <ListSkeleton rows={2} />;
  if (q.error) return <ErrorState error={q.error} retry={() => void q.refetch()} />;
  const list = q.data ?? [];
  return (
    <div className="space-y-3">
      {owner ? (
        <div className="flex justify-end">
          <Button size="sm" onClick={() => setCreating(true)}>
            <Plus /> New trigger
          </Button>
        </div>
      ) : null}
      {list.length ? (
        <ul className="divide-y rounded-lg border">
          {list.map((t) => (
            <li key={t.id} className="flex items-center gap-3 p-3">
              <div className="min-w-0 flex-1">
                <div className="flex items-center gap-2 text-sm font-medium">
                  <span className="truncate">{t.name}</span>
                  <Badge variant="outline">{t.kind}</Badge>
                  {!t.enabled ? <Badge variant="secondary">off</Badge> : null}
                </div>
                <div className="truncate text-xs text-muted-foreground">
                  {t.kind === "cron" ? t.schedule : t.event} · creates “{t.template.title}”
                  {t.nextFireAt ? ` · next ${ago(t.nextFireAt).replace(/^(.*) ago$/, "in $1")}` : ""}
                  {t.lastFiredAt ? ` · last ${ago(t.lastFiredAt)}` : ""}
                </div>
              </div>
              {owner ? (
                <Button variant="ghost" size="icon" aria-label={`Delete ${t.name}`} onClick={() => setTarget({ id: t.id, name: t.name })}>
                  <Trash2 />
                </Button>
              ) : null}
            </li>
          ))}
        </ul>
      ) : (
        <Empty title="No triggers">{owner ? "A trigger creates tasks on a schedule or from a GitHub event." : "A project owner can add triggers."}</Empty>
      )}
      <CreateTriggerDialog projectId={projectId} open={creating} onOpenChange={setCreating} />
      <AlertDialog open={!!target} onOpenChange={(o) => !o && setTarget(undefined)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete “{target?.name}”?</AlertDialogTitle>
            <AlertDialogDescription>It stops creating tasks. Tasks it already created stay.</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={() => target && del.mutate(target.id)}>Delete</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-lg border p-3">
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className="text-lg font-semibold tabular-nums">{value}</div>
    </div>
  );
}

function Learning({ projectId }: { projectId: string }) {
  const clusters = useLearning<LearningCluster[]>(projectId, "clusters");
  const exp = useLearning<LearningExperiment>(projectId, "experiment");
  // 404 answers null: the server runs without the learning feature.
  if (clusters.isLoading || exp.isLoading) return null;
  if (clusters.data === null || exp.data === null) return <p className="text-sm text-muted-foreground">Learning views are off on this server.</p>;
  return (
    <div className="space-y-4">
      <section>
        <h3 className="mb-2 text-sm font-medium">Recurring failures</h3>
        {clusters.data?.length ? (
          <ul className="divide-y rounded-lg border">
            {clusters.data.map((c) => (
              <li key={c.signature} className="p-3 text-sm">
                <div className="flex items-center gap-2">
                  <Badge variant="outline">{c.count}x</Badge>
                  <span className="min-w-0 flex-1 truncate">{c.titles?.[0] ?? c.signature}</span>
                  <span className="text-xs text-muted-foreground">{usd(c.usd)} spent</span>
                </div>
                {c.sample ? <p className="mt-1 line-clamp-2 text-xs text-muted-foreground">{c.sample}</p> : null}
              </li>
            ))}
          </ul>
        ) : (
          <p className="text-sm text-muted-foreground">No failure has recurred.</p>
        )}
      </section>
      <section>
        <h3 className="mb-2 text-sm font-medium">Prompt experiment</h3>
        {exp.data?.standings?.length ? (
          <table className="w-full text-sm">
            <thead className="text-left text-xs text-muted-foreground">
              <tr>
                <th className="py-1 font-normal">Variant</th>
                <th className="font-normal">Runs</th>
                <th className="font-normal">Success</th>
                <th className="font-normal">Mean cost</th>
              </tr>
            </thead>
            <tbody>
              {exp.data.standings.map((s) => (
                <tr key={s.variant} className="border-t">
                  <td className="py-1">
                    {s.variant || "control"} {exp.data?.winner === s.variant ? <Badge className="ml-1">winner</Badge> : null}
                  </td>
                  <td>{s.n}</td>
                  <td>{Math.round(s.rate * 100)}%</td>
                  <td>{usd(s.meanUsd)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : (
          <p className="text-sm text-muted-foreground">No experiment results yet.</p>
        )}
      </section>
    </div>
  );
}

function Report({ projectId }: { projectId: string }) {
  const [days, setDays] = useState(7);
  const q = useFactoryReport(projectId, days);
  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h3 className="text-sm font-medium">Spend</h3>
        <Select value={String(days)} onValueChange={(v) => setDays(Number(v))}>
          <SelectTrigger size="sm" className="w-36" aria-label="Period">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="7">Last 7 days</SelectItem>
            <SelectItem value="30">Last 30 days</SelectItem>
            <SelectItem value="90">Last 90 days</SelectItem>
          </SelectContent>
        </Select>
      </div>
      {q.isLoading ? <ListSkeleton rows={2} /> : q.error || !q.data ? <ErrorState error={q.error} retry={() => void q.refetch()} /> : <ReportBody r={q.data} />}
      <Learning projectId={projectId} />
    </div>
  );
}

function ReportBody({ r }: { r: NonNullable<ReturnType<typeof useFactoryReport>["data"]> }) {
  const states = Object.entries(r.ByState ?? {}).filter(([, n]) => n > 0);
  return (
    <div className="space-y-4">
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <Stat label="Spent" value={usd(r.Total.usd)} />
        <Stat label="Tokens" value={r.Total.tokens.toLocaleString()} />
        <Stat label="Runs" value={String(r.Total.runs)} />
        <Stat label="Tasks" value={String(states.reduce((n, [, c]) => n + c, 0))} />
      </div>
      {r.Total.unpriced ? <p className="rounded-md bg-warning/15 p-2 text-sm">{r.Total.unpriced} runs used a model with no known price: their cost shows as $0, it is not free.</p> : null}
      {states.length ? (
        <div className="flex flex-wrap gap-2">
          {states.map(([s, n]) => (
            <Badge key={s} variant="outline">
              {TASK_STATE_LABEL[s as FactoryState] ?? s}: {n}
            </Badge>
          ))}
        </div>
      ) : null}
      {r.Top?.length ? (
        <section>
          <h3 className="mb-2 text-sm font-medium">Most expensive tasks</h3>
          <ul className="divide-y rounded-lg border">
            {r.Top.map((t) => (
              <li key={t.ID} className="flex items-center gap-3 p-3 text-sm">
                <TaskStateBadge state={t.State} />
                <span className="min-w-0 flex-1 truncate">{t.Title}</span>
                <span className="tabular-nums">{usd(t.Spent.usd)}</span>
              </li>
            ))}
          </ul>
        </section>
      ) : (
        <p className="text-sm text-muted-foreground">No spend in this period.</p>
      )}
    </div>
  );
}

/** The project's "Tasks" tab: tasks, triggers and the spend report. */
export function TasksTab({ projectId, role }: { projectId: string; role: Role }) {
  return (
    <Tabs defaultValue="tasks">
      <TabsList>
        <TabsTrigger value="tasks">List</TabsTrigger>
        <TabsTrigger value="triggers">Triggers</TabsTrigger>
        <TabsTrigger value="report">Report</TabsTrigger>
      </TabsList>
      <TabsContent value="tasks" className="mt-4">
        <TaskList projectId={projectId} role={role} />
      </TabsContent>
      <TabsContent value="triggers" className="mt-4">
        <Triggers projectId={projectId} owner={role === "owner"} />
      </TabsContent>
      <TabsContent value="report" className="mt-4">
        <Report projectId={projectId} />
      </TabsContent>
    </Tabs>
  );
}
