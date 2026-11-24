// A workflow run: the graph with the state of each step (kept current by the live feed), the selected step's task, result and
// log, and the controls: pause, resume, cancel, rerun from a step, retry, take over, and a gate's approve, send back and add context.
import { Link } from "@tanstack/react-router";
import { formatDistanceToNowStrict } from "date-fns";
import { ArrowLeft, ExternalLink, GitPullRequest, Pause, Play, RotateCcw, Square } from "lucide-react";
import { useState } from "react";
import { useTask } from "@/api/factory";
import { useGateAction, useRunAction, useWorkflowRun, type WorkflowRun } from "@/api/workflows";
import { ErrorState, ListSkeleton } from "@/components/app/common";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { Canvas, STEP_STATE_LABEL } from "./Canvas";

const ago = (iso: string) => `${formatDistanceToNowStrict(new Date(iso))} ago`;

export const RUN_STATE_LABEL: Record<string, string> = { running: "Running", paused: "Paused", done: "Done", failed: "Failed", cancelled: "Cancelled" };
export const RUN_STATE_TONE: Record<string, string> = {
  running: "bg-primary/15 text-primary",
  paused: "bg-warning text-warning-foreground",
  done: "bg-success/15 text-success",
  failed: "bg-destructive/15 text-destructive",
  cancelled: "bg-muted text-muted-foreground",
};

export const RunBadge = ({ state }: { state: string }) => <span className={`rounded-full px-2 py-0.5 text-xs font-medium ${RUN_STATE_TONE[state] ?? "bg-muted"}`}>{RUN_STATE_LABEL[state] ?? state}</span>;

/** Approve, send back or add context to a waiting gate: used by the run view and by the inbox. */
export function GateActions({ runId, nodeId, onDone, compact }: { runId: string; nodeId: string; onDone?: () => void; compact?: boolean }) {
  const act = useGateAction();
  const [text, setText] = useState("");
  const [mode, setMode] = useState<"send-back" | "add-context" | null>(null);
  const run = (action: "approve" | "send-back" | "add-context") =>
    act.mutate({ runId, nodeId, action, text: text.trim() }, { onSuccess: () => (setText(""), setMode(null), onDone?.()) });
  const needsText = mode === "send-back" || mode === "add-context";
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap gap-2">
        <Button size="sm" disabled={act.isPending} onClick={() => run("approve")}>
          Approve
        </Button>
        <Button size="sm" variant={mode === "send-back" ? "secondary" : "outline"} aria-expanded={mode === "send-back"} onClick={() => setMode(mode === "send-back" ? null : "send-back")}>
          Send back
        </Button>
        <Button size="sm" variant={mode === "add-context" ? "secondary" : "outline"} aria-expanded={mode === "add-context"} onClick={() => setMode(mode === "add-context" ? null : "add-context")}>
          Add context
        </Button>
      </div>
      {mode ? (
        <div className="space-y-2">
          <Label htmlFor={`gate-text-${runId}-${nodeId}`} className="text-xs">
            {mode === "send-back" ? "What must change (the step before runs again with this)" : "A note the next steps are given"}
          </Label>
          <Textarea id={`gate-text-${runId}-${nodeId}`} rows={compact ? 2 : 3} value={text} onChange={(e) => setText(e.target.value)} />
          <div className="flex gap-2">
            <Button size="sm" disabled={act.isPending || (needsText && !text.trim())} onClick={() => run(mode)}>
              {mode === "send-back" ? "Send it back" : "Add the note"}
            </Button>
            <Button size="sm" variant="ghost" onClick={() => setMode(null)}>
              Cancel
            </Button>
          </div>
        </div>
      ) : null}
      {!mode ? (
        <Textarea rows={1} placeholder="Comment with your approval (optional)" aria-label="Approval comment" value={text} onChange={(e) => setText(e.target.value)} className="text-xs" />
      ) : null}
    </div>
  );
}

function TextDialog({ title, description, label, action, required, onClose, onSubmit }: { title: string; description: string; label: string; action: string; required?: boolean; onClose: () => void; onSubmit: (t: string) => void }) {
  const [t, setT] = useState("");
  return (
    <AlertDialog open onOpenChange={(o) => !o && onClose()}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{title}</AlertDialogTitle>
          <AlertDialogDescription>{description}</AlertDialogDescription>
        </AlertDialogHeader>
        <div className="space-y-1.5">
          <Label htmlFor="wf-dialog-text">{label}</Label>
          <Textarea id="wf-dialog-text" rows={5} value={t} onChange={(e) => setT(e.target.value)} />
        </div>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction disabled={required && !t.trim()} onClick={() => onSubmit(t.trim())}>
            {action}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

function StepPanel({ run, nodeId, projectId, canSteer, mayDecide }: { run: WorkflowRun; nodeId: string; projectId: string; canSteer: boolean; mayDecide: boolean }) {
  const step = run.steps.find((s) => s.nodeId === nodeId);
  const node = run.graph.find((n) => n.id === nodeId);
  const act = useRunAction();
  const task = useTask(projectId, step?.taskId);
  const [dlg, setDlg] = useState<"rerun" | "takeover" | null>(null);
  if (!step || !node) return null;
  const events = (run.events ?? []).filter((e) => e.nodeId === nodeId).slice(-12).reverse();
  const finished = step.state === "done";
  const submit = (action: "rerun" | "takeover") => (t: string) => {
    setDlg(null);
    act.mutate({ runId: run.id, action, nodeId, ...(action === "rerun" ? { feedback: t } : { result: t }) });
  };
  return (
    <section className="space-y-3 text-sm" aria-label={`Step ${node.title || node.id}`}>
      <div className="flex flex-wrap items-center gap-2">
        <h3 className="font-medium">{node.title || node.id}</h3>
        <Badge variant="secondary">{STEP_STATE_LABEL[step.state] ?? step.state}</Badge>
        {step.round > 1 ? <span className="text-xs text-muted-foreground">attempt {step.round}</span> : null}
        {step.manual ? <Badge variant="outline">done by hand</Badge> : null}
      </div>
      {step.taskId ? (
        <div className="flex flex-wrap items-center gap-2 text-xs">
          <Link to="/p/$projectId" params={{ projectId }} search={{ tab: "tasks", task: step.taskId }} className="inline-flex items-center gap-1 text-primary hover:underline">
            Open task <ExternalLink className="size-3" />
          </Link>
          {step.taskState ? <span className="text-muted-foreground">task {step.taskState}</span> : null}
          {step.prUrl ? (
            <a href={step.prUrl} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 text-primary hover:underline">
              <GitPullRequest className="size-3" /> Pull request
            </a>
          ) : null}
        </div>
      ) : null}
      {step.direct ? (
        <div className="space-y-1">
          <p className="text-xs text-muted-foreground">
            Ran in the run's sandbox, no model involved{step.exitCode != null ? ` (exit code ${step.exitCode})` : step.state === "running" ? " (running)" : ""}.
          </p>
          {step.url ? (
            <a href={step.url} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 text-xs text-primary hover:underline">
              <ExternalLink className="size-3" /> Open the preview
            </a>
          ) : null}
          {step.log ? (
            <pre aria-label="Step output" className="max-h-64 overflow-auto whitespace-pre-wrap rounded-md bg-muted p-2 font-mono text-xs">{step.log}</pre>
          ) : null}
        </div>
      ) : null}
      {step.childRunId ? (
        <p className="text-xs text-muted-foreground">Runs the workflow as run {step.childRunId.slice(0, 8)} ({step.taskState}).</p>
      ) : null}
      {step.note && !finished ? <p className="rounded-md bg-warning/10 p-2 text-xs">{step.note}</p> : null}
      {step.error ? <pre className="max-h-40 overflow-auto whitespace-pre-wrap rounded-md bg-destructive/10 p-2 text-xs text-destructive">{step.error}</pre> : null}
      {step.feedback ? (
        <div className="space-y-1">
          <Label className="text-xs">Feedback this attempt was given</Label>
          <p className="max-h-24 overflow-auto whitespace-pre-wrap rounded-md bg-muted p-2 text-xs">{step.feedback}</p>
        </div>
      ) : null}
      {step.result ? (
        <div className="space-y-1">
          <Label className="text-xs">Result</Label>
          <pre className="max-h-64 overflow-auto whitespace-pre-wrap rounded-md bg-muted p-2 text-xs">{step.result}</pre>
        </div>
      ) : null}
      {Object.entries(step.outputs ?? {}).map(([k, v]) => (
        <div key={k} className="space-y-1">
          <Label className="text-xs">Deliverable: {k}</Label>
          <pre className="max-h-48 overflow-auto whitespace-pre-wrap rounded-md bg-muted p-2 text-xs">{v || "(not reported)"}</pre>
        </div>
      ))}
      {(step.context ?? []).length ? (
        <div className="space-y-1">
          <Label className="text-xs">Notes added at the gate</Label>
          <ul className="space-y-1 text-xs">
            {step.context!.map((c, i) => (
              <li key={i} className="rounded-md bg-muted p-2">
                {c}
              </li>
            ))}
          </ul>
        </div>
      ) : null}

      {step.state === "waiting" ? (
        mayDecide ? (
          <div className="space-y-2 rounded-md border border-warning/60 p-3">
            <p className="text-xs">
              {node.kind === "gate" ? `This gate waits for you. Approvers: ${(node.approvers ?? []).join(", ")}.` : `This command waits for your approval before it runs: ${node.command ?? ""}`}
            </p>
            <GateActions runId={run.id} nodeId={nodeId} />
          </div>
        ) : (
          <p className="rounded-md border p-2 text-xs text-muted-foreground">Waiting for {node.kind === "gate" ? (node.approvers ?? []).join(", ") : "a project owner"} to decide.</p>
        )
      ) : null}

      {canSteer ? (
        <div className="flex flex-wrap gap-2">
          <Button size="sm" variant="outline" disabled={act.isPending} onClick={() => setDlg("rerun")}>
            <RotateCcw /> Rerun from here
          </Button>
          {step.state === "failed" || step.state === "cancelled" ? (
            <Button size="sm" variant="outline" disabled={act.isPending} onClick={() => act.mutate({ runId: run.id, action: "retry", nodeId })}>
              Retry
            </Button>
          ) : null}
          {!finished ? (
            <Button size="sm" variant="outline" disabled={act.isPending} onClick={() => setDlg("takeover")}>
              Take over
            </Button>
          ) : null}
        </div>
      ) : null}

      {task.data?.events?.length ? (
        <details className="text-xs">
          <summary className="cursor-pointer text-muted-foreground">Task log ({task.data.events.length})</summary>
          <ul className="mt-1 max-h-48 space-y-1 overflow-auto">
            {task.data.events.slice(-30).map((e) => (
              <li key={e.id} className="flex gap-2">
                <span className="shrink-0 text-muted-foreground">{e.kind}</span>
                <span className="min-w-0 break-words">{e.text}</span>
              </li>
            ))}
          </ul>
        </details>
      ) : null}
      {events.length ? (
        <details className="text-xs" open>
          <summary className="cursor-pointer text-muted-foreground">History of this step</summary>
          <ul className="mt-1 space-y-1">
            {events.map((e, i) => (
              <li key={i} className="flex gap-2">
                <span className="shrink-0 text-muted-foreground">{ago(e.at)}</span>
                <span className="min-w-0 break-words">
                  {e.actor ? `${e.actor}: ` : ""}
                  {e.text}
                </span>
              </li>
            ))}
          </ul>
        </details>
      ) : null}

      {dlg === "rerun" ? (
        <TextDialog
          title={`Rerun from ${node.title || node.id}?`}
          description="This step and every step after it are reset and run again; steps that do not depend on it keep their results. Running tasks are cancelled."
          label="What should this attempt do differently? (optional)"
          action="Rerun"
          onClose={() => setDlg(null)}
          onSubmit={submit("rerun")}
        />
      ) : null}
      {dlg === "takeover" ? (
        <TextDialog
          title={`Take over ${node.title || node.id}`}
          description="You do this step yourself. Its task is cancelled, and your result is what the next steps are given."
          label="The result"
          action="Mark as done"
          required
          onClose={() => setDlg(null)}
          onSubmit={submit("takeover")}
        />
      ) : null}
    </section>
  );
}

/** One run, live. */
export function RunView({ projectId, runId, onBack }: { projectId: string; runId: string; onBack: () => void }) {
  const q = useWorkflowRun(projectId, runId);
  const act = useRunAction();
  const [selected, setSelected] = useState<string | null>(null);
  const [confirmCancel, setConfirmCancel] = useState(false);
  if (q.isLoading) return <ListSkeleton rows={3} />;
  if (q.error || !q.data) return <ErrorState error={q.error} retry={() => void q.refetch()} />;
  const { run, gates, canSteer } = q.data;
  const states = Object.fromEntries(run.steps.map((s) => [s.nodeId, s.state]));
  const waiting = gates.filter((g) => g.mayDecide);
  const focus = selected ?? waiting[0]?.nodeId ?? run.steps.find((s) => s.state === "failed")?.nodeId ?? run.steps.find((s) => s.state === "running")?.nodeId ?? run.graph[0]?.id ?? null;
  const live = run.state === "running" || run.state === "paused";
  const inputs = Object.entries(run.inputs ?? {}).filter(([, v]) => v);
  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-2">
        <Button variant="ghost" size="sm" onClick={onBack}>
          <ArrowLeft /> Runs
        </Button>
        <h2 className="text-base font-medium">{run.workflowName}</h2>
        <RunBadge state={run.state} />
        <span className="text-xs text-muted-foreground">
          v{run.workflowVersion} · started {ago(run.createdAt)} by {run.owner}
        </span>
        {canSteer && live ? (
          <div className="ml-auto flex gap-2">
            {run.state === "running" ? (
              <Button size="sm" variant="outline" disabled={act.isPending} onClick={() => act.mutate({ runId: run.id, action: "pause" })}>
                <Pause /> Pause
              </Button>
            ) : (
              <Button size="sm" variant="outline" disabled={act.isPending} onClick={() => act.mutate({ runId: run.id, action: "resume" })}>
                <Play /> Resume
              </Button>
            )}
            <Button size="sm" variant="outline" disabled={act.isPending} onClick={() => setConfirmCancel(true)}>
              <Square /> Cancel run
            </Button>
          </div>
        ) : null}
      </div>
      {run.error ? <p className="rounded-md bg-destructive/10 p-2 text-sm text-destructive">{run.error}</p> : null}
      {run.state === "paused" ? <p className="rounded-md bg-warning/10 p-2 text-sm">Paused: steps that are running finish, but no new step starts until you resume.</p> : null}
      {inputs.length ? (
        <dl className="grid gap-x-4 gap-y-1 text-xs sm:grid-cols-[auto_1fr]">
          {inputs.map(([k, v]) => (
            <div key={k} className="contents">
              <dt className="text-muted-foreground">{k}</dt>
              <dd className="line-clamp-2 break-words">{v}</dd>
            </div>
          ))}
        </dl>
      ) : null}
      <div className="grid gap-4 lg:grid-cols-[minmax(0,1fr)_24rem]">
        <Canvas className="h-[26rem]" nodes={run.graph} states={states} selected={focus} readOnly onSelect={setSelected} />
        <div className="rounded-lg border p-4">
          {focus ? <StepPanel run={run} nodeId={focus} projectId={projectId} canSteer={canSteer} mayDecide={!!gates.find((g) => g.nodeId === focus)?.mayDecide} /> : <p className="text-sm text-muted-foreground">Select a step.</p>}
        </div>
      </div>
      <details className="rounded-lg border p-3 text-xs">
        <summary className="cursor-pointer text-sm font-medium">Run log ({run.events?.length ?? 0})</summary>
        <ul className="mt-2 max-h-72 space-y-1 overflow-auto">
          {[...(run.events ?? [])].reverse().map((e, i) => (
            <li key={i} className="flex gap-2">
              <span className="w-20 shrink-0 text-muted-foreground">{ago(e.at)}</span>
              {e.nodeId ? <span className="shrink-0 font-medium">{e.nodeId}</span> : null}
              <span className="min-w-0 break-words">
                {e.actor ? `${e.actor}: ` : ""}
                {e.text}
              </span>
            </li>
          ))}
        </ul>
      </details>
      <AlertDialog open={confirmCancel} onOpenChange={setConfirmCancel}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Cancel this run?</AlertDialogTitle>
            <AlertDialogDescription>Running tasks are stopped and the steps that did not start are cancelled. Finished steps keep their results, and you can rerun from any step afterwards.</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Keep running</AlertDialogCancel>
            <AlertDialogAction onClick={() => act.mutate({ runId: run.id, action: "cancel" })}>Cancel the run</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
