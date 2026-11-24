// The Workflows tab of a project: its procedures (SOPs) as versioned graphs, the builtin templates to start from, the designer, and
// the runs with their live state and controls. A workflow is a graph of steps (an agent, a command, a human gate, another
// workflow); a run turns each step into a factory task in dependency order.
import { EmptyGuide } from "@/components/app/EmptyGuide";
import { useNavigate } from "@tanstack/react-router";
import { useProjectSearch } from "@/lib/place";
import { formatDistanceToNowStrict } from "date-fns";
import { Copy, FilePlus2, Play, Trash2 } from "lucide-react";
import { useState } from "react";
import { useRequirements } from "@/api/queries";
import type { Role } from "@/api/types";
import {
  useCreateWorkflow, useDeleteWorkflow, useDuplicateWorkflow, useFromTemplate, useStartRun, useWorkflowRuns, useWorkflowTemplates, useWorkflows, type Workflow, type WorkflowRun,
} from "@/api/workflows";
import { Empty, ErrorState, ListSkeleton } from "@/components/app/common";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { Designer } from "@/components/workflows/Designer";
import { RunBadge, RunView } from "@/components/workflows/RunView";

const ago = (iso: string) => `${formatDistanceToNowStrict(new Date(iso))} ago`;

/** The inputs a run is started with: what the workflow declares, plus an optional requirement. */
export function StartRunDialog({ projectId, workflow, onClose, onStarted }: { projectId: string; workflow: Workflow; onClose: () => void; onStarted: (r: WorkflowRun) => void }) {
  const start = useStartRun(projectId);
  const reqs = useRequirements(projectId);
  const inputs = workflow.inputs ?? [];
  const [values, setValues] = useState<Record<string, string>>(() => Object.fromEntries(inputs.map((i) => [i.name, i.default ?? ""])));
  const [req, setReq] = useState("");
  const missing = inputs.filter((i) => i.required && !values[i.name]?.trim());
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Run {workflow.name}</DialogTitle>
          <DialogDescription>Each step becomes a task of the agent factory, in order; steps that do not depend on each other run together.</DialogDescription>
        </DialogHeader>
        <div className="space-y-3">
          {inputs.map((i) => (
            <div key={i.name} className="space-y-1.5">
              <Label htmlFor={`run-in-${i.name}`}>
                {i.label || i.name}
                {i.required ? " *" : ""}
              </Label>
              {i.long ? (
                <Textarea id={`run-in-${i.name}`} rows={4} value={values[i.name] ?? ""} onChange={(e) => setValues({ ...values, [i.name]: e.target.value })} />
              ) : (
                <Input id={`run-in-${i.name}`} value={values[i.name] ?? ""} onChange={(e) => setValues({ ...values, [i.name]: e.target.value })} />
              )}
            </div>
          ))}
          {reqs.data?.length ? (
            <div className="space-y-1.5">
              <Label htmlFor="run-req">Requirement (optional)</Label>
              <select id="run-req" className="h-9 w-full rounded-md border border-input bg-transparent px-2 text-sm" value={req} onChange={(e) => setReq(e.target.value)}>
                <option value="">None</option>
                {reqs.data.map((r) => (
                  <option key={r.id} value={r.id}>
                    {r.title}
                  </option>
                ))}
              </select>
            </div>
          ) : null}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button disabled={start.isPending || missing.length > 0} onClick={() => start.mutate({ workflowId: workflow.id, inputs: values, requirementId: req || undefined }, { onSuccess: (r) => (onStarted(r), onClose()) })}>
            <Play /> Start the run
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function WorkflowCard({ w, canEdit, onOpen, onRun }: { w: Workflow; canEdit: boolean; onOpen: () => void; onRun: () => void }) {
  const dup = useDuplicateWorkflow(w.projectId);
  const del = useDeleteWorkflow(w.projectId);
  const [confirm, setConfirm] = useState(false);
  const published = w.status === "published";
  return (
    <Card>
      <CardHeader className="pb-2">
        <CardTitle className="flex items-start justify-between gap-2 text-sm">
          <button className="min-w-0 truncate text-left hover:underline" onClick={onOpen}>
            {w.name}
          </button>
          <Badge variant={published ? "default" : "secondary"}>{published ? `published v${w.publishedVersion}` : "draft"}</Badge>
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-3 text-xs text-muted-foreground">
        {w.description ? <p className="line-clamp-2">{w.description}</p> : null}
        <p>
          {w.nodes.length} steps · v{w.version} · changed {ago(w.updatedAt)}
          {w.updatedBy ? ` by ${w.updatedBy}` : ""}
        </p>
        <div className="flex flex-wrap gap-2">
          <Button size="sm" variant="outline" onClick={onOpen}>
            {canEdit ? "Open designer" : "View"}
          </Button>
          {canEdit ? (
            <Button size="sm" disabled={!published} title={published ? undefined : "Publish the workflow to run it"} onClick={onRun}>
              <Play /> Run
            </Button>
          ) : null}
          {canEdit ? (
            <>
              <Button size="sm" variant="ghost" aria-label={`Duplicate ${w.name}`} onClick={() => dup.mutate({ id: w.id })}>
                <Copy />
              </Button>
              <Button size="sm" variant="ghost" aria-label={`Delete ${w.name}`} onClick={() => setConfirm(true)}>
                <Trash2 />
              </Button>
            </>
          ) : null}
        </div>
      </CardContent>
      <AlertDialog open={confirm} onOpenChange={setConfirm}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete {w.name}?</AlertDialogTitle>
            <AlertDialogDescription>The workflow and its versions are removed. Runs that already started keep going.</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Keep it</AlertDialogCancel>
            <AlertDialogAction onClick={() => del.mutate(w.id)}>Delete</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Card>
  );
}

function Templates({ projectId, onCreated }: { projectId: string; onCreated: (w: Workflow) => void }) {
  const q = useWorkflowTemplates();
  const copy = useFromTemplate(projectId);
  if (!q.data?.length) return null;
  return (
    <section aria-label="Templates" className="space-y-3">
      <h2 className="text-sm font-medium text-muted-foreground">Start from a template</h2>
      <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
        {q.data.map((t) => (
          <Card key={t.id}>
            <CardHeader className="pb-2">
              <CardTitle className="text-sm">{t.name}</CardTitle>
            </CardHeader>
            <CardContent className="space-y-3 text-xs text-muted-foreground">
              <p>{t.description}</p>
              <p>{t.workflow.nodes.map((n) => n.title || n.id).join(" → ")}</p>
              <Button size="sm" variant="outline" disabled={copy.isPending} onClick={() => copy.mutate({ template: t.id }, { onSuccess: onCreated })} aria-label={`Use this template: ${t.name}`}>
                <FilePlus2 /> Use this template
              </Button>
            </CardContent>
          </Card>
        ))}
      </div>
    </section>
  );
}

function RunsList({ projectId, onOpen }: { projectId: string; onOpen: (id: string) => void }) {
  const q = useWorkflowRuns(projectId);
  if (q.isLoading) return <ListSkeleton rows={3} />;
  if (q.error) return <ErrorState error={q.error} retry={() => void q.refetch()} />;
  if (!q.data?.length) return <EmptyGuide title="No runs yet" what="Publish a workflow and run it: each run is listed here with the state of its steps." slug="workflows" />;
  return (
    <ul className="divide-y rounded-lg border">
      {q.data.map((r) => {
        const waiting = r.steps.filter((s) => s.state === "waiting").length;
        const done = r.steps.filter((s) => s.state === "done").length;
        return (
          <li key={r.id}>
            <button className="flex w-full items-center gap-3 p-3 text-left hover:bg-accent" onClick={() => onOpen(r.id)}>
              <span className="min-w-0 flex-1 truncate text-sm">{r.workflowName}</span>
              <RunBadge state={r.state} />
              {waiting ? <Badge className="bg-warning text-warning-foreground">needs a decision</Badge> : null}
              <span className="text-xs text-muted-foreground">
                {done}/{r.steps.length} steps
              </span>
              <span className="w-24 text-right text-xs text-muted-foreground">{ago(r.createdAt)}</span>
            </button>
          </li>
        );
      })}
    </ul>
  );
}

/** The Workflows tab. */
export function WorkflowsPanel({ projectId, role }: { projectId: string; role: Role }) {
  const list = useWorkflows(projectId);
  const create = useCreateWorkflow(projectId);
  const navigate = useNavigate();
  const search = useProjectSearch();
  const [running, setRunning] = useState<Workflow>();
  const [section, setSection] = useState<"workflows" | "runs">(search.run ? "runs" : "workflows");
  const canEdit = role !== "viewer";
  const go = (s: { wf?: string; run?: string }) => void navigate({ to: "/p/$projectId", params: { projectId }, search: { tab: "workflows", ...s }, replace: true });

  if (list.isLoading) return <ListSkeleton />;
  if (list.error) return <ErrorState error={list.error} retry={() => void list.refetch()} />;
  if (list.data === null) return <Empty title="Workflows are not enabled on this server" />;
  const all = list.data ?? [];

  const open = all.find((w) => w.id === search.wf);
  if (open) return <Designer key={open.id} projectId={projectId} workflow={open} canEdit={canEdit} onBack={() => go({})} />;
  if (search.run) return <RunView projectId={projectId} runId={search.run} onBack={() => go({})} />;

  const blank = () =>
    create.mutate(
      { name: "New workflow", description: "", inputs: [], nodes: [{ id: "plan", kind: "agent", title: "Plan", role: "planner", prompt: "", pos: { x: 40, y: 40 } }] },
      { onSuccess: (w) => go({ wf: w.id }) },
    );
  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-center gap-3">
        <span className="flex-1" />
        {canEdit ? (
          <Button onClick={blank} disabled={create.isPending}>
            <FilePlus2 /> New workflow
          </Button>
        ) : null}
      </div>
      <Tabs value={section} onValueChange={(v) => setSection(v as "workflows" | "runs")}>
        <TabsList>
          <TabsTrigger value="workflows">Workflows ({all.length})</TabsTrigger>
          <TabsTrigger value="runs">Runs</TabsTrigger>
        </TabsList>
        <TabsContent value="workflows" className="mt-4 space-y-6">
          {all.length ? (
            <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
              {all.map((w) => (
                <WorkflowCard key={w.id} w={w} canEdit={canEdit} onOpen={() => go({ wf: w.id })} onRun={() => setRunning(w)} />
              ))}
            </div>
          ) : (
            <EmptyGuide title="No workflows yet" what="Start from a template below, or build one from scratch." slug="workflow-from-template">
              {canEdit ? <Button variant="outline" onClick={blank} disabled={create.isPending}>Start with a blank workflow</Button> : null}
            </EmptyGuide>
          )}
          {canEdit ? <Templates projectId={projectId} onCreated={(w) => go({ wf: w.id })} /> : null}
        </TabsContent>
        <TabsContent value="runs" className="mt-4">
          <RunsList projectId={projectId} onOpen={(id) => go({ run: id })} />
        </TabsContent>
      </Tabs>
      {running ? <StartRunDialog projectId={projectId} workflow={running} onClose={() => setRunning(undefined)} onStarted={(r) => go({ run: r.id })} /> : null}
    </div>
  );
}
