import { HelpButton } from "@/components/app/help";
// The workflow designer: the canvas, the inspector of the selected step, the checks, the version history and publishing.
import { ArrowLeft, ChevronDown, Copy, History, LayoutGrid, Plus, Rocket, Save, ShieldCheck, Trash2 } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { toast } from "sonner";
import { useRoles } from "@/api/roles";
import { issuesOf, useDuplicateWorkflow, usePublishWorkflow, useRestoreWorkflow, useUpdateWorkflow, useWorkflowHistory, useWorkflows, validateWorkflow, type Workflow } from "@/api/workflows";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { Canvas } from "./Canvas";
import {
  KINDS, ancestors, autoLayout, connect, disconnect, inputsText, isCommand, kindLabel, localIssues, moveNode, newNode, nextPos, parseInputs, parseList, removeNode, renameNode, sameGraph,
  type Issue, type Kind, type Node,
} from "./graph";

const BUILTIN_ROLES = ["planner", "coder", "reviewer", "tester", "explorer"];
const POLICIES = ["", "supervised", "sandboxed", "autonomous"];
const selectCls = "h-9 w-full rounded-md border border-input bg-transparent px-2 text-sm shadow-xs outline-none focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50";

function Field({ label, children, hint }: { label: string; children: React.ReactNode; hint?: string }) {
  return (
    <div className="space-y-1.5">
      <Label>{label}</Label>
      {children}
      {hint ? <p className="text-xs text-muted-foreground">{hint}</p> : null}
    </div>
  );
}

/** The inspector of one step. */
export function NodeInspector({
  node, nodes, roles, workflows, inputNames, onChange, onRename, onRemove, disabled,
}: {
  node: Node;
  nodes: Node[];
  roles: string[];
  workflows: Workflow[];
  inputNames: string[];
  onChange: (n: Node) => void;
  onRename: (to: string) => string | undefined;
  onRemove: () => void;
  disabled?: boolean;
}) {
  const [id, setId] = useState(node.id);
  const [idError, setIdError] = useState<string>();
  useEffect(() => setId(node.id), [node.id]);
  const set = (p: Partial<Node>) => onChange({ ...node, ...p });
  const earlier = ancestors(nodes, node.id);
  const f = node.onFailure ?? {};
  const action = f.action || "fail";
  return (
    <div className="space-y-4" aria-label="Step settings">
      <div className="flex items-center justify-between gap-2">
        <h3 className="text-sm font-medium">{kindLabel(node.kind)} step</h3>
        <Button size="sm" variant="ghost" disabled={disabled} onClick={onRemove} aria-label={`Remove step ${node.id}`}>
          <Trash2 /> Remove
        </Button>
      </div>
      <Field label="Title">
        <Input value={node.title ?? ""} disabled={disabled} onChange={(e) => set({ title: e.target.value })} aria-label="Title" />
      </Field>
      <Field label="Id" hint={idError ?? "Other steps refer to this one as {{nodes.<id>.result}}."}>
        <Input
          value={id}
          disabled={disabled}
          aria-label="Id"
          aria-invalid={!!idError}
          onChange={(e) => setId(e.target.value)}
          onBlur={() => {
            const err = onRename(id);
            setIdError(err);
            if (err) setId(node.id);
          }}
        />
      </Field>
      {node.kind === "agent" ? (
        <>
          <Field label="Role">
            <select className={selectCls} value={node.role || "coder"} disabled={disabled} onChange={(e) => set({ role: e.target.value })} aria-label="Role">
              {[...new Set([...roles, node.role || "coder"])].map((r) => (
                <option key={r} value={r}>
                  {r}
                </option>
              ))}
            </select>
          </Field>
          <Field label="Prompt" hint="What the agent is asked to do. {{inputs.name}}, {{nodes.id.result}} and {{requirement.title}} are filled in when the run starts.">
            <Textarea rows={8} value={node.prompt ?? ""} disabled={disabled} onChange={(e) => set({ prompt: e.target.value })} aria-label="Prompt" />
          </Field>
          <Field label="Deliverables" hint="Names of the artifacts the step produces, separated by commas. The agent ends its report with a ### section for each.">
            <Input value={(node.outputs ?? []).join(", ")} disabled={disabled} onChange={(e) => set({ outputs: parseList(e.target.value) })} aria-label="Deliverables" />
          </Field>
          <div className="grid grid-cols-2 gap-3">
            <Field label="Model">
              <Input value={node.model ?? ""} placeholder="default" disabled={disabled} onChange={(e) => set({ model: e.target.value })} aria-label="Model" />
            </Field>
            <Field label="Policy">
              <select className={selectCls} value={node.policy ?? ""} disabled={disabled} onChange={(e) => set({ policy: e.target.value })} aria-label="Policy">
                {POLICIES.map((p) => (
                  <option key={p} value={p}>
                    {p || "default"}
                  </option>
                ))}
              </select>
            </Field>
          </div>
          <Field label="Budget (USD)" hint="Most the step may spend, all attempts included. Empty: the project default.">
            <Input
              inputMode="decimal"
              value={node.budget?.maxUsd ?? ""}
              disabled={disabled}
              onChange={(e) => set({ budget: e.target.value.trim() && Number(e.target.value) > 0 ? { maxUsd: Number(e.target.value) } : undefined })}
              aria-label="Budget"
            />
          </Field>
          <label className="flex items-center justify-between gap-3 text-sm">
            Work in the repository
            <Switch checked={!!node.useRepo} disabled={disabled} onCheckedChange={(v) => set({ useRepo: v })} aria-label="Work in the repository" />
          </label>
          {node.useRepo ? (
            <label className="flex items-center justify-between gap-3 text-sm">
              Done only when the pull request is merged
              <Switch checked={!!node.waitMerge} disabled={disabled} onCheckedChange={(v) => set({ waitMerge: v })} aria-label="Wait for the merge" />
            </label>
          ) : null}
        </>
      ) : null}
      {isCommand(node.kind) ? (
        <>
          <Field label="Command" hint="Run in the task's sandbox; the step succeeds when it exits 0.">
            <Textarea rows={4} className="font-mono text-xs" value={node.command ?? ""} disabled={disabled} onChange={(e) => set({ command: e.target.value })} aria-label="Command" />
          </Field>
          <label className="flex items-center justify-between gap-3 text-sm">
            Run in the repository checkout
            <Switch checked={!!node.useRepo} disabled={disabled} onCheckedChange={(v) => set({ useRepo: v })} aria-label="Run in the repository" />
          </label>
        </>
      ) : null}
      {node.kind === "gate" ? (
        <p className="flex items-center gap-1 text-xs text-muted-foreground">
          A gate waits here until a person decides. <HelpButton slug="gates" />
        </p>
      ) : null}
      {node.kind === "gate" ? (
        <Field label="Approvers" hint="Logins, role:member or role:owner, separated by commas. Project owners can always decide.">
          <Input value={(node.approvers ?? []).join(", ")} disabled={disabled} onChange={(e) => set({ approvers: parseList(e.target.value) })} aria-label="Approvers" />
        </Field>
      ) : null}
      {node.kind === "subworkflow" ? (
        <>
          <Field label="Workflow" hint="Only published workflows can be run.">
            <select className={selectCls} value={node.workflowId ?? ""} disabled={disabled} onChange={(e) => set({ workflowId: e.target.value })} aria-label="Workflow to run">
              <option value="">Choose…</option>
              {workflows.map((w) => (
                <option key={w.id} value={w.id} disabled={w.status !== "published"}>
                  {w.name}
                  {w.status !== "published" ? " (draft)" : ""}
                </option>
              ))}
            </select>
          </Field>
          <Field label="Inputs" hint="One name=value per line; values can use {{…}} like a prompt.">
            <Textarea rows={3} className="font-mono text-xs" value={inputsText(node.inputs)} disabled={disabled} onChange={(e) => set({ inputs: parseInputs(e.target.value) })} aria-label="Subworkflow inputs" />
          </Field>
        </>
      ) : null}
      <div className="space-y-3 rounded-md border p-3">
        <h4 className="text-xs font-medium uppercase tracking-wide text-muted-foreground">{node.kind === "gate" ? "When sent back" : "When it fails"}</h4>
        {node.kind === "gate" ? null : (
          <select
            className={selectCls}
            value={action}
            disabled={disabled}
            aria-label="On failure"
            onChange={(e) => set({ onFailure: { ...f, action: e.target.value, target: e.target.value === "rework" ? (f.target || earlier[0] || "") : f.target } })}
          >
            <option value="fail">Fail the run</option>
            <option value="continue">Carry on without it</option>
            <option value="rework">Send back to an earlier step</option>
          </select>
        )}
        {action === "rework" || node.kind === "gate" ? (
          <div className="grid grid-cols-2 gap-3">
            <Field label="Back to">
              <select
                className={selectCls}
                value={f.target ?? ""}
                disabled={disabled}
                aria-label="Back to"
                onChange={(e) => set({ onFailure: { ...f, action: node.kind === "gate" ? "rework" : action, target: e.target.value } })}
              >
                <option value="">{node.kind === "gate" ? "The step before" : "Choose…"}</option>
                {earlier.map((x) => (
                  <option key={x} value={x}>
                    {x}
                  </option>
                ))}
              </select>
            </Field>
            <Field label="At most">
              <Input
                inputMode="numeric"
                value={f.maxRounds ?? ""}
                placeholder="3"
                disabled={disabled}
                onChange={(e) => set({ onFailure: { ...f, action: node.kind === "gate" ? "rework" : action, maxRounds: Number(e.target.value) > 0 ? Math.min(20, Math.floor(Number(e.target.value))) : undefined } })}
                aria-label="Rework rounds"
              />
            </Field>
          </div>
        ) : null}
        {node.kind !== "gate" && action === "rework" ? <p className="text-xs text-muted-foreground">The failure text goes to the step it is sent back to; every step after it runs again.</p> : null}
      </div>
      {inputNames.length ? <p className="text-xs text-muted-foreground">Inputs: {inputNames.map((n) => `{{inputs.${n}}}`).join(" ")}</p> : null}
    </div>
  );
}

function HistoryDialog({ pid, w, onClose, onRestore }: { pid: string; w: Workflow; onClose: () => void; onRestore: (v: number) => void }) {
  const q = useWorkflowHistory(pid, w.id, w.version);
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Version history</DialogTitle>
          <DialogDescription>The last ten versions. Restoring one makes it a new draft; nothing is lost.</DialogDescription>
        </DialogHeader>
        <ul className="max-h-80 divide-y overflow-auto rounded-md border text-sm">
          {(q.data ?? []).map((v) => (
            <li key={v.version} className="flex items-center gap-2 p-2">
              <span className="font-medium">v{v.version}</span>
              <span className="min-w-0 flex-1 truncate text-xs text-muted-foreground">
                {v.changedBy ?? "someone"} · {new Date(v.at).toLocaleString()} · {v.workflow.nodes.length} steps
              </span>
              <Button size="sm" variant="outline" onClick={() => onRestore(v.version)} aria-label={`Restore version ${v.version}`}>
                Restore
              </Button>
            </li>
          ))}
          {q.data && !q.data.length ? <li className="p-3 text-muted-foreground">No earlier versions.</li> : null}
        </ul>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            Close
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/** Edits one workflow. */
export function Designer({ projectId, workflow, canEdit, onBack }: { projectId: string; workflow: Workflow; canEdit: boolean; onBack: () => void }) {
  const [saved, setSaved] = useState(workflow);
  const [name, setName] = useState(workflow.name);
  const [description, setDescription] = useState(workflow.description ?? "");
  const [inputs, setInputs] = useState(workflow.inputs ?? []);
  const [nodes, setNodes] = useState<Node[]>(workflow.nodes);
  const [selected, setSelected] = useState<string | null>(null);
  const inspectorRef = useRef<HTMLElement>(null);
  // On a phone the inspector sits below the canvas: bring it into view when a step is chosen.
  useEffect(() => {
    if (selected && typeof window !== "undefined" && window.innerWidth < 1024) inspectorRef.current?.scrollIntoView?.({ block: "nearest" });
  }, [selected]);
  const [edge, setEdge] = useState<{ from: string; to: string } | null>(null);
  const [connecting, setConnecting] = useState<string | null>(null);
  const [server, setServer] = useState<Issue[]>([]);
  const [history, setHistory] = useState(false);
  const roles = useRoles(projectId);
  const list = useWorkflows(projectId);
  const update = useUpdateWorkflow(projectId);
  const publish = usePublishWorkflow(projectId);
  const restore = useRestoreWorkflow(projectId);
  const duplicate = useDuplicateWorkflow(projectId);

  const roleNames = useMemo(() => (roles.data ? roles.data.filter((r) => r.enabled).map((r) => r.name) : BUILTIN_ROLES), [roles.data]);
  const inputNames = inputs.map((i) => i.name);
  const dirty = !sameGraph({ name, description, inputs, nodes }, { name: saved.name, description: saved.description ?? "", inputs: saved.inputs ?? [], nodes: saved.nodes });
  const local = useMemo(() => localIssues(name, nodes, inputNames), [name, nodes, inputNames.join(",")]); // eslint-disable-line react-hooks/exhaustive-deps
  const issues = [...local, ...server.filter((s) => !local.some((l) => l.nodeId === s.nodeId && l.message === s.message))];
  const sel = nodes.find((n) => n.id === selected);
  const editable = canEdit;
  const edit = (next: Node[]) => {
    setNodes(next);
    setServer([]);
  };
  const body = () => ({ name, description, inputs, nodes, version: saved.version });

  const load = (w: Workflow) => {
    setSaved(w);
    setName(w.name);
    setDescription(w.description ?? "");
    setInputs(w.inputs ?? []);
    setNodes(w.nodes);
    setServer([]);
  };

  const save = async (): Promise<Workflow | undefined> => {
    try {
      const w = await update.mutateAsync({ id: saved.id, body: body() });
      setSaved(w);
      setServer([]);
      return w;
    } catch (e) {
      setServer(issuesOf(e));
      return undefined;
    }
  };

  const check = async () => {
    try {
      const is = await validateWorkflow(projectId, body(), saved.id);
      setServer(is);
      if (!is.length) toast.success("No problems found");
    } catch (e) {
      toast.error((e as Error).message);
    }
  };

  const doPublish = async () => {
    const w = dirty ? await save() : saved;
    if (!w) return;
    try {
      const p = await publish.mutateAsync({ id: w.id, version: w.version });
      setSaved(p);
      setServer([]);
    } catch (e) {
      setServer(issuesOf(e));
    }
  };

  const add = (kind: Kind) => {
    const n = newNode(kind, nodes, nextPos(nodes));
    const prev = selected && nodes.some((x) => x.id === selected) ? selected : undefined;
    edit([...nodes, prev ? { ...n, dependsOn: [prev] } : n]);
    setSelected(n.id);
    setEdge(null);
  };

  const doConnect = (from: string, to: string) => {
    const r = connect(nodes, from, to);
    setConnecting(null);
    if (r.error) toast.error(r.error);
    else edit(r.nodes);
  };

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-2">
        <Button variant="ghost" size="sm" onClick={onBack}>
          <ArrowLeft /> Workflows
        </Button>
        <Input className="h-9 max-w-xs flex-1 font-medium" value={name} disabled={!editable} onChange={(e) => setName(e.target.value)} aria-label="Workflow name" />
        <Badge variant={saved.status === "published" && !dirty ? "default" : "secondary"}>{saved.status === "published" ? (dirty ? "published, unsaved changes" : `published v${saved.publishedVersion}`) : "draft"}</Badge>
        <span className="text-xs text-muted-foreground">v{saved.version}</span>
        <div className="ml-auto flex flex-wrap gap-2">
          <Button variant="outline" size="sm" onClick={() => setHistory(true)}>
            <History /> History
          </Button>
          <Button variant="outline" size="sm" onClick={() => duplicate.mutate({ id: saved.id })} disabled={!editable}>
            <Copy /> Duplicate
          </Button>
          <Button variant="outline" size="sm" onClick={() => void check()}>
            <ShieldCheck /> Check
          </Button>
          <Button variant="outline" size="sm" disabled={!editable || !dirty || update.isPending} onClick={() => void save()}>
            <Save /> Save
          </Button>
          <Button size="sm" disabled={!editable || update.isPending || publish.isPending || (!dirty && saved.status === "published")} onClick={() => void doPublish()}>
            <Rocket /> Publish
          </Button>
        </div>
      </div>

      {editable ? (
        <div className="flex flex-wrap items-center gap-2" role="toolbar" aria-label="Add a step">
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button size="sm" variant="outline">
                <Plus /> Add step <ChevronDown />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="start" className="w-72">
              {KINDS.map((k) => (
                <DropdownMenuItem key={k.kind} onSelect={() => add(k.kind)} className="flex-col items-start gap-0">
                  <span>{k.label}</span>
                  <span className="text-xs text-muted-foreground">{k.hint}</span>
                </DropdownMenuItem>
              ))}
            </DropdownMenuContent>
          </DropdownMenu>
          <Button size="sm" variant="ghost" onClick={() => edit(autoLayout(nodes))}>
            <LayoutGrid /> Tidy
          </Button>
          {connecting ? (
            <span className="text-xs text-primary">
              Click the step that should wait for {connecting}.{" "}
              <button className="underline" onClick={() => setConnecting(null)}>
                Cancel
              </button>
            </span>
          ) : (
            <span className="text-xs text-muted-foreground">Drag a step to move it. Click the dot on its right, then another step, to connect them.</span>
          )}
        </div>
      ) : null}

      <div className="grid gap-4 lg:grid-cols-[minmax(0,1fr)_22rem]">
        <div className="min-w-0 space-y-3">
          <Canvas
            className="h-[28rem]"
            nodes={nodes}
            selected={selected}
            connecting={connecting}
            selectedEdge={edge}
            readOnly={!editable}
            onSelect={(id) => (setSelected(id), setEdge(null))}
            onSelectEdge={(e) => (setEdge(e), setSelected(null))}
            onMove={(id, x, y) => edit(moveNode(nodes, id, x, y))}
            onStartConnect={(id) => setConnecting((c) => (c === id ? null : id))}
            onConnect={doConnect}
          />
          {edge && editable ? (
            <div className="flex items-center gap-2 rounded-md border p-2 text-sm">
              <span>
                {edge.to} waits for {edge.from}.
              </span>
              <Button size="sm" variant="outline" onClick={() => (edit(disconnect(nodes, edge.from, edge.to)), setEdge(null))}>
                Remove connection
              </Button>
            </div>
          ) : null}
          <section aria-label="Problems" className="space-y-1 text-sm">
            {issues.length ? (
              <ul className="space-y-1 rounded-md border border-destructive/40 bg-destructive/5 p-3" role="alert">
                {issues.map((i, k) => (
                  <li key={k}>
                    {i.nodeId ? (
                      <button className="font-medium underline" onClick={() => setSelected(i.nodeId!)}>
                        {i.nodeId}
                      </button>
                    ) : null}
                    {i.nodeId ? ": " : ""}
                    {i.message}
                  </li>
                ))}
              </ul>
            ) : (
              <p className="text-xs text-muted-foreground">No problems found in the graph.</p>
            )}
          </section>
        </div>

        <aside ref={inspectorRef} className="space-y-4 rounded-lg border p-4" aria-label="Inspector">
          {sel ? (
            <>
              <NodeInspector
                node={sel}
                nodes={nodes}
                roles={roleNames}
                workflows={(list.data ?? []).filter((w) => w.id !== saved.id)}
                inputNames={inputNames}
                disabled={!editable}
                onChange={(n) => edit(nodes.map((x) => (x.id === n.id ? n : x)))}
                onRename={(to) => {
                  const r = renameNode(nodes, sel.id, to);
                  if (r.error) return r.error;
                  edit(r.nodes);
                  setSelected(to);
                  return undefined;
                }}
                onRemove={() => (edit(removeNode(nodes, sel.id)), setSelected(null))}
              />
              {sel.dependsOn?.length ? (
                <div className="space-y-1.5">
                  <Label>Waits for</Label>
                  <ul className="space-y-1">
                    {sel.dependsOn.map((d) => (
                      <li key={d} className="flex items-center justify-between rounded-md bg-muted px-2 py-1 text-sm">
                        {d}
                        {editable ? (
                          <button className="text-xs underline" onClick={() => edit(disconnect(nodes, d, sel.id))} aria-label={`Stop waiting for ${d}`}>
                            Remove
                          </button>
                        ) : null}
                      </li>
                    ))}
                  </ul>
                </div>
              ) : null}
            </>
          ) : (
            <div className="space-y-4">
              <h3 className="text-sm font-medium">Workflow</h3>
              <Field label="Description">
                <Textarea rows={3} value={description} disabled={!editable} onChange={(e) => setDescription(e.target.value)} aria-label="Description" />
              </Field>
              <div className="space-y-2">
                <Label>Inputs</Label>
                <p className="text-xs text-muted-foreground">Asked when a run starts; steps use them as {"{{inputs.name}}"}.</p>
                {inputs.map((inp, i) => (
                  <div key={i} className="flex items-center gap-2">
                    <Input value={inp.name} placeholder="name" disabled={!editable} onChange={(e) => setInputs(inputs.map((x, j) => (j === i ? { ...x, name: e.target.value } : x)))} aria-label={`Input ${i + 1} name`} />
                    <Input value={inp.default ?? ""} placeholder="default" disabled={!editable} onChange={(e) => setInputs(inputs.map((x, j) => (j === i ? { ...x, default: e.target.value } : x)))} aria-label={`Input ${i + 1} default`} />
                    <label className="flex items-center gap-1 text-xs">
                      <Switch checked={!!inp.required} disabled={!editable} onCheckedChange={(v) => setInputs(inputs.map((x, j) => (j === i ? { ...x, required: v } : x)))} aria-label={`Input ${i + 1} required`} />
                      required
                    </label>
                    {editable ? (
                      <Button size="icon" variant="ghost" aria-label={`Remove input ${inp.name}`} onClick={() => setInputs(inputs.filter((_, j) => j !== i))}>
                        <Trash2 />
                      </Button>
                    ) : null}
                  </div>
                ))}
                {editable ? (
                  <Button size="sm" variant="outline" onClick={() => setInputs([...inputs, { name: "", required: false }])}>
                    <Plus /> Add input
                  </Button>
                ) : null}
              </div>
              <p className="text-xs text-muted-foreground">Select a step to edit it.</p>
            </div>
          )}
        </aside>
      </div>

      {history ? (
        <HistoryDialog
          pid={projectId}
          w={saved}
          onClose={() => setHistory(false)}
          onRestore={(v) =>
            restore.mutate(
              { id: saved.id, version: v },
              {
                onSuccess: (w) => {
                  load(w);
                  setHistory(false);
                  setSelected(null);
                },
              },
            )
          }
        />
      ) : null}
    </div>
  );
}
