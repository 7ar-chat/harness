// Scheduled agent tasks: a prompt the agent runs by itself on a cron or interval schedule. The project tab and the personal page share the panel.
import { Link } from "@tanstack/react-router";
import { formatDistanceToNowStrict } from "date-fns";
import { ChevronDown, ChevronRight, Clock, Pencil, Play, Plus, Trash2 } from "lucide-react";
import { useEffect, useState } from "react";
import { isUnsupported } from "@/api/client";
import { useModels } from "@/api/queries";
import { useDeleteSchedule, useRunSchedule, useSaveSchedule, useSchedulePreview, useScheduleRuns, useSchedules, type ScheduleInput } from "@/api/schedulesUsage";
import type { ScheduleRun, ScheduleView } from "@/api/types";
import { Empty, ErrorState, ListSkeleton, Spinner } from "@/components/app/common";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";

/** The schedules people reach for most; anything else is typed as cron ("0 9 * * 1-5") or "@every 90m". */
export const CRON_PRESETS: { label: string; cron: string }[] = [
  { label: "Every 15 minutes", cron: "*/15 * * * *" },
  { label: "Every hour", cron: "0 * * * *" },
  { label: "Every day at 09:00", cron: "0 9 * * *" },
  { label: "Weekdays at 09:00", cron: "0 9 * * 1-5" },
  { label: "Every Monday at 09:00", cron: "0 9 * * 1" },
  { label: "The 1st of the month at 09:00", cron: "0 9 1 * *" },
];

const localZone = () => {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch {
    return "UTC";
  }
};

const zones = (): string[] => {
  try {
    return (Intl as unknown as { supportedValuesOf?: (k: string) => string[] }).supportedValuesOf?.("timeZone") ?? [];
  } catch {
    return [];
  }
};

/** A moment as people read it in a zone: "Tue, 24 Nov, 09:00". An unknown zone falls back to the viewer's. */
export function formatIn(iso: string, timeZone: string): string {
  const opts: Intl.DateTimeFormatOptions = { weekday: "short", day: "numeric", month: "short", hour: "2-digit", minute: "2-digit", hour12: false };
  try {
    return new Date(iso).toLocaleString(undefined, { ...opts, timeZone });
  } catch {
    return new Date(iso).toLocaleString(undefined, opts);
  }
}

const rel = (iso: string) => formatDistanceToNowStrict(new Date(iso), { addSuffix: true });
const money = (usd: number) => (usd >= 1 ? `$${usd.toFixed(2)}` : `$${usd.toFixed(4)}`);
const dur = (a: string, b?: string) => {
  if (!b) return "";
  const s = Math.max(0, Math.round((new Date(b).getTime() - new Date(a).getTime()) / 1000));
  return s < 90 ? `${s}s` : `${Math.round(s / 60)} min`;
};

const STATUS_STYLE: Record<string, string> = {
  succeeded: "bg-success/15 text-success",
  failed: "bg-destructive/15 text-destructive",
  timeout: "bg-destructive/15 text-destructive",
  running: "bg-orange-500/15 text-orange-600 dark:text-orange-400",
};

export function RunStatus({ status }: { status: string }) {
  return (
    <Badge variant="outline" className={STATUS_STYLE[status] ?? "text-muted-foreground"}>
      {status === "running" ? <Spinner className="size-3" /> : null}
      {status}
    </Badge>
  );
}

function RunHistory({ projectId, schedule }: { projectId?: string; schedule: ScheduleView }) {
  const runs = useScheduleRuns(projectId, schedule.id, true);
  if (runs.isLoading) return <ListSkeleton rows={2} />;
  if (runs.error) return <ErrorState error={runs.error} retry={() => void runs.refetch()} />;
  if (!runs.data?.length) return <p className="px-1 py-2 text-sm text-muted-foreground">No runs yet.</p>;
  return (
    <table className="w-full text-sm" aria-label={`Runs of ${schedule.name}`}>
      <thead className="text-left text-xs text-muted-foreground">
        <tr>
          <th className="py-1 pr-3 font-normal">Started</th>
          <th className="py-1 pr-3 font-normal">Status</th>
          <th className="py-1 pr-3 font-normal">Took</th>
          <th className="py-1 pr-3 text-right font-normal">Cost</th>
          <th className="py-1 font-normal">Session</th>
        </tr>
      </thead>
      <tbody>
        {runs.data.map((r) => (
          <RunRow key={r.id} run={r} zone={schedule.timezone} />
        ))}
      </tbody>
    </table>
  );
}

function RunRow({ run, zone }: { run: ScheduleRun; zone: string }) {
  const tokens = run.usage.inputTokens + run.usage.outputTokens;
  return (
    <tr className="border-t align-top">
      <td className="py-1.5 pr-3 whitespace-nowrap" title={run.startedAt}>
        {formatIn(run.startedAt, zone)}
        {run.trigger === "manual" ? <span className="ml-1.5 text-xs text-muted-foreground">manual</span> : null}
      </td>
      <td className="py-1.5 pr-3">
        <RunStatus status={run.status} />
        {run.error ? <p className="mt-0.5 max-w-sm text-xs text-muted-foreground">{run.error}</p> : null}
      </td>
      <td className="py-1.5 pr-3 whitespace-nowrap">{dur(run.startedAt, run.finishedAt)}</td>
      <td className="py-1.5 pr-3 text-right whitespace-nowrap tabular-nums" title={`${tokens.toLocaleString()} tokens`}>
        {tokens ? money(run.usage.costUsd) : ""}
        {run.usage.unpriced ? <span title="Some of the models have no price: the cost is a lower bound"> *</span> : null}
      </td>
      <td className="py-1.5">
        {run.threadId ? (
          <Link to="/t/$threadId" params={{ threadId: run.threadId }} className="text-primary hover:underline">
            Open session
          </Link>
        ) : null}
      </td>
    </tr>
  );
}

function ScheduleItem({ projectId, schedule, onEdit }: { projectId?: string; schedule: ScheduleView; onEdit: () => void }) {
  const [open, setOpen] = useState(false);
  const [confirm, setConfirm] = useState(false);
  const save = useSaveSchedule(projectId);
  const run = useRunSchedule(projectId);
  const del = useDeleteSchedule(projectId);
  const s = schedule;
  const busy = s.lastStatus === "running";
  return (
    <li className="rounded-lg border">
      <div className="flex flex-wrap items-start gap-3 p-3">
        <button type="button" onClick={() => setOpen(!open)} aria-expanded={open} aria-label={`${open ? "Hide" : "Show"} runs of ${s.name}`} className="mt-0.5 text-muted-foreground hover:text-foreground">
          {open ? <ChevronDown className="size-4" /> : <ChevronRight className="size-4" />}
        </button>
        <div className="min-w-0 flex-1 space-y-1">
          <div className="flex flex-wrap items-center gap-2">
            <span className="font-medium">{s.name}</span>
            {s.policy === "readonly" ? <Badge variant="secondary">read-only</Badge> : null}
            {s.lastStatus ? <RunStatus status={s.lastStatus} /> : null}
          </div>
          <p className="line-clamp-2 text-sm text-muted-foreground">{s.prompt}</p>
          <p className="flex flex-wrap items-center gap-x-3 gap-y-0.5 text-xs text-muted-foreground">
            <span className="inline-flex items-center gap-1">
              <Clock className="size-3" />
              <code>{s.cron}</code> · {s.timezone}
            </span>
            {s.enabled && s.nextRunAt ? <span title={s.nextRunAt}>next {formatIn(s.nextRunAt, s.timezone)} ({rel(s.nextRunAt)})</span> : <span>{s.enabled ? "never fires again" : "paused"}</span>}
            {s.lastRunAt ? <span>last {rel(s.lastRunAt)}</span> : null}
            {s.lastRun && s.lastRun.usage.costUsd > 0 ? <span>{money(s.lastRun.usage.costUsd)}</span> : null}
            <span>as {s.runAs}</span>
          </p>
          {s.lastRun?.threadId ? (
            <Link to="/t/$threadId" params={{ threadId: s.lastRun.threadId }} className="text-xs text-primary hover:underline">
              Open the last session
            </Link>
          ) : null}
        </div>
        {s.canEdit ? (
          <div className="flex shrink-0 items-center gap-2">
            <Switch checked={s.enabled} onCheckedChange={(enabled) => save.mutate({ id: s.id, body: { enabled } })} disabled={save.isPending} aria-label={`Enable ${s.name}`} />
            <Button size="sm" variant="outline" disabled={run.isPending || busy} onClick={() => run.mutate(s.id)} aria-label={`Run ${s.name} now`}>
              <Play /> Run now
            </Button>
            <Button size="icon" variant="ghost" onClick={onEdit} aria-label={`Edit ${s.name}`}>
              <Pencil />
            </Button>
            <Button size="icon" variant="ghost" onClick={() => setConfirm(true)} aria-label={`Delete ${s.name}`}>
              <Trash2 />
            </Button>
          </div>
        ) : null}
      </div>
      {open ? (
        <div className="overflow-x-auto border-t px-3 pb-3 pl-10">
          <RunHistory projectId={projectId} schedule={s} />
        </div>
      ) : null}
      <AlertDialog open={confirm} onOpenChange={setConfirm}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete “{s.name}”?</AlertDialogTitle>
            <AlertDialogDescription>It stops running and its history goes with it. The sessions it started stay.</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={() => del.mutate(s.id)}>Delete</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </li>
  );
}

interface Draft {
  name: string;
  prompt: string;
  cron: string;
  timezone: string;
  model: string;
  policy: "default" | "readonly";
  timeoutMinutes: string;
  reuseSession: boolean;
  enabled: boolean;
}

const draftOf = (s?: ScheduleView): Draft => ({
  name: s?.name ?? "",
  prompt: s?.prompt ?? "",
  cron: s?.cron ?? "0 9 * * 1-5",
  timezone: s?.timezone ?? localZone(),
  model: s?.model ?? "",
  policy: s?.policy === "readonly" ? "readonly" : "default",
  timeoutMinutes: String(s?.timeoutMinutes ?? 30),
  reuseSession: s?.reuseSession ?? false,
  enabled: s?.enabled ?? true,
});

/** The next fire times of the expression, in the schedule's zone: the check that it means what the person thinks. */
function NextTimes({ cron, timezone }: { cron: string; timezone: string }) {
  const preview = useSchedulePreview(cron, timezone);
  if (preview.error) return <p className="text-sm text-destructive" role="alert">{preview.error.message}</p>;
  if (!preview.data) return <p className="text-xs text-muted-foreground">Checking…</p>;
  if (!preview.data.times.length) return <p className="text-sm text-destructive">This schedule never fires.</p>;
  return (
    <div className="space-y-1">
      <p className="text-xs text-muted-foreground">Next {preview.data.times.length} runs ({timezone})</p>
      <ol className="grid gap-x-4 text-sm tabular-nums sm:grid-cols-2" aria-label="Next runs">
        {preview.data.times.map((t) => (
          <li key={t}>{formatIn(t, timezone)}</li>
        ))}
      </ol>
    </div>
  );
}

export function ScheduleDialog({ projectId, schedule, open, onClose }: { projectId?: string; schedule?: ScheduleView; open: boolean; onClose: () => void }) {
  const save = useSaveSchedule(projectId);
  const models = useModels().data;
  const [d, setD] = useState<Draft>(() => draftOf(schedule));
  useEffect(() => {
    if (open) setD(draftOf(schedule));
  }, [open, schedule]);
  const set = <K extends keyof Draft>(k: K, v: Draft[K]) => setD((x) => ({ ...x, [k]: v }));
  const minutes = Number(d.timeoutMinutes);
  const timeoutOk = Number.isInteger(minutes) && minutes >= 1 && minutes <= 240;
  const ready = d.name.trim() !== "" && d.prompt.trim() !== "" && d.cron.trim() !== "" && timeoutOk;
  const preset = CRON_PRESETS.find((p) => p.cron === d.cron.trim())?.cron ?? "custom";
  const submit = () => {
    const body: ScheduleInput = {
      name: d.name.trim(), prompt: d.prompt.trim(), cron: d.cron.trim(), timezone: d.timezone.trim() || "UTC", model: d.model, policy: d.policy,
      timeoutMinutes: minutes, reuseSession: d.reuseSession, enabled: d.enabled,
    };
    save.mutate({ id: schedule?.id, body }, { onSuccess: onClose });
  };
  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-xl">
        <form
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault();
            if (ready) submit();
          }}
        >
          <DialogHeader>
            <DialogTitle>{schedule ? "Edit schedule" : "New schedule"}</DialogTitle>
            <DialogDescription>The agent runs this prompt by itself, in a session of its own. It runs as you, the last person to save it.</DialogDescription>
          </DialogHeader>
          <div className="space-y-1.5">
            <Label htmlFor="sc-name">Name</Label>
            <Input id="sc-name" value={d.name} onChange={(e) => set("name", e.target.value)} placeholder="Morning triage" required />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="sc-prompt">Prompt</Label>
            <Textarea id="sc-prompt" rows={5} value={d.prompt} onChange={(e) => set("prompt", e.target.value)} placeholder="Summarise the pull requests merged since yesterday and flag anything that needs a decision." required />
          </div>
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="space-y-1.5">
              <Label htmlFor="sc-preset">Repeat</Label>
              <Select value={preset} onValueChange={(v) => v !== "custom" && set("cron", v)}>
                <SelectTrigger id="sc-preset" className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {CRON_PRESETS.map((p) => (
                    <SelectItem key={p.cron} value={p.cron}>
                      {p.label}
                    </SelectItem>
                  ))}
                  <SelectItem value="custom">Custom</SelectItem>
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="sc-cron">Cron or interval</Label>
              <Input id="sc-cron" value={d.cron} onChange={(e) => set("cron", e.target.value)} placeholder="0 9 * * 1-5 or @every 90m" className="font-mono" required />
            </div>
            <div className="space-y-1.5 sm:col-span-2">
              <Label htmlFor="sc-tz">Time zone</Label>
              <Input id="sc-tz" list="sc-zones" value={d.timezone} onChange={(e) => set("timezone", e.target.value)} placeholder="Europe/Paris" />
              <datalist id="sc-zones">
                {zones().map((z) => (
                  <option key={z} value={z} />
                ))}
              </datalist>
              <p className="text-xs text-muted-foreground">minute hour day-of-month month day-of-week. Missed runs (the server was down) are skipped, not replayed.</p>
            </div>
          </div>
          <NextTimes cron={d.cron} timezone={d.timezone || "UTC"} />
          <div className="grid gap-3 sm:grid-cols-3">
            <div className="space-y-1.5">
              <Label htmlFor="sc-model">Model</Label>
              <Select value={d.model || "default"} onValueChange={(v) => set("model", v === "default" ? "" : v)}>
                <SelectTrigger id="sc-model" className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="default">Server default</SelectItem>
                  {(models?.models ?? []).map((m) => (
                    <SelectItem key={m} value={m}>
                      {m}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="sc-policy">Permissions</Label>
              <Select value={d.policy} onValueChange={(v) => set("policy", v as Draft["policy"])}>
                <SelectTrigger id="sc-policy" className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="default">Normal</SelectItem>
                  <SelectItem value="readonly">Read-only (plan mode)</SelectItem>
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="sc-timeout">Stop after (minutes)</Label>
              <Input id="sc-timeout" type="number" min={1} max={240} value={d.timeoutMinutes} onChange={(e) => set("timeoutMinutes", e.target.value)} aria-invalid={!timeoutOk} />
              {!timeoutOk ? <p className="text-xs text-destructive">1 to 240.</p> : null}
            </div>
          </div>
          <div className="flex items-center justify-between gap-4">
            <div>
              <Label htmlFor="sc-reuse">Keep one conversation</Label>
              <p className="text-xs text-muted-foreground">Every run continues the same session instead of starting a new one.</p>
            </div>
            <Switch id="sc-reuse" checked={d.reuseSession} onCheckedChange={(v) => set("reuseSession", v)} />
          </div>
          <div className="flex items-center justify-between gap-4">
            <Label htmlFor="sc-enabled">Enabled</Label>
            <Switch id="sc-enabled" checked={d.enabled} onCheckedChange={(v) => set("enabled", v)} />
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" disabled={!ready || save.isPending}>
              {schedule ? "Save" : "Create"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

/** The schedules of a project (`projectId`), or the caller's own without one. `canEdit` says whether to offer "New schedule" (the rows follow what the server says). */
export function SchedulesPanel({ projectId, canEdit }: { projectId?: string; canEdit: boolean }) {
  const q = useSchedules(projectId);
  const [editing, setEditing] = useState<ScheduleView | "new" | null>(null);
  if (q.error && isUnsupported(q.error)) return <Empty title="No scheduled tasks">This server has no scheduled tasks.</Empty>;
  if (q.isLoading) return <ListSkeleton rows={2} />;
  if (q.error || !q.data) return <ErrorState error={q.error} retry={() => void q.refetch()} />;
  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="max-w-2xl text-sm text-muted-foreground">
          Put an agent on a recurring job: it runs the prompt on its schedule, as an ordinary session you can open, with its cost on the Usage page. A run stops at its time limit.
        </p>
        {canEdit ? (
          <Button onClick={() => setEditing("new")}>
            <Plus /> New schedule
          </Button>
        ) : null}
      </div>
      {q.data.length ? (
        <ul className="space-y-3" aria-label="Schedules">
          {q.data.map((s) => (
            <ScheduleItem key={s.id} projectId={projectId} schedule={s} onEdit={() => setEditing(s)} />
          ))}
        </ul>
      ) : (
        <Empty title="No schedules yet">{canEdit ? "Create one to have an agent do something every morning, every week or every hour." : "Members can create schedules."}</Empty>
      )}
      <ScheduleDialog projectId={projectId} schedule={editing && editing !== "new" ? editing : undefined} open={editing !== null} onClose={() => setEditing(null)} />
    </div>
  );
}

/** The personal schedules, at /schedules. */
export function SchedulesPage() {
  return (
    <ScrollArea className="h-full">
      <div className="mx-auto w-full max-w-5xl space-y-4 p-4 md:p-8">
        <h1 className="text-xl font-semibold">Scheduled tasks</h1>
        <SchedulesPanel canEdit />
      </div>
    </ScrollArea>
  );
}
