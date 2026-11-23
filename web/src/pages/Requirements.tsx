// The project's requirements: what the project wants done, followed from the idea to the merged pull request.
import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import { AlertTriangle, ArrowLeft, ExternalLink, GitPullRequest, Pencil, Plus, Trash2 } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import {
  useCreateRequirement,
  useDeleteRequirement,
  useDispatchRequirement,
  useGithub,
  useGitHosts,
  useImportIssues,
  useImportRequirements,
  usePatchRequirement,
  usePutRequirementSettings,
  useRequirement,
  useRequirementSettings,
  useRequirements,
} from "@/api/queries";
import type { Requirement, Role } from "@/api/types";
import { Empty, ErrorState, ListSkeleton } from "@/components/app/common";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { cn } from "@/lib/utils";

export const STATUSES = [
  { id: "backlog", label: "Backlog", dot: "bg-muted-foreground/40" },
  { id: "ready", label: "Ready", dot: "bg-warning" },
  { id: "in_progress", label: "In progress", dot: "bg-warning" },
  { id: "in_review", label: "In review", dot: "bg-primary" },
  { id: "done", label: "Done", dot: "bg-success" },
  { id: "rejected", label: "Rejected", dot: "bg-destructive" },
] as const;
const statusOf = (id: string) => STATUSES.find((s) => s.id === id) ?? STATUSES[0];
const labelsOf = (s: string) => [...new Set(s.split(",").map((x) => x.trim()).filter(Boolean))];

const selectClass = "h-9 rounded-md border border-input bg-transparent px-2 text-sm shadow-xs";

function StatusBadge({ status }: { status: string }) {
  const s = statusOf(status);
  return (
    <Badge variant="outline" className="gap-1.5">
      <span className={cn("size-2 rounded-full", s.dot)} />
      {s.label}
    </Badge>
  );
}

/** Create or edit one requirement. */
function EditDialog({ projectId, requirement, open, onOpenChange }: { projectId: string; requirement?: Requirement; open: boolean; onOpenChange: (o: boolean) => void }) {
  const create = useCreateRequirement(projectId);
  const patch = usePatchRequirement(projectId);
  const [title, setTitle] = useState("");
  const [body, setBody] = useState("");
  const [priority, setPriority] = useState("0");
  const [labels, setLabels] = useState("");
  const [status, setStatus] = useState("backlog");
  useEffect(() => {
    if (!open) return;
    setTitle(requirement?.title ?? "");
    setBody(requirement?.body ?? "");
    setPriority(String(requirement?.priority ?? 0));
    setLabels((requirement?.labels ?? []).join(", "));
    setStatus(requirement?.status ?? "backlog");
  }, [open, requirement]);
  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    const input = { title: title.trim(), body, priority: Number.parseInt(priority, 10) || 0, labels: labelsOf(labels), status };
    const done = { onSuccess: () => onOpenChange(false) };
    if (requirement) patch.mutate({ id: requirement.id, patch: input }, done);
    else create.mutate(input, done);
  };
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <form onSubmit={submit} className="space-y-3">
          <DialogHeader>
            <DialogTitle>{requirement ? "Edit requirement" : "New requirement"}</DialogTitle>
            <DialogDescription>What the project wants done. Dispatch it to an agent when it is ready.</DialogDescription>
          </DialogHeader>
          <div className="space-y-1.5">
            <Label htmlFor="req-title">Title</Label>
            <Input id="req-title" value={title} onChange={(e) => setTitle(e.target.value)} required autoFocus />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="req-body">Description (markdown)</Label>
            <Textarea id="req-body" value={body} onChange={(e) => setBody(e.target.value)} rows={6} />
          </div>
          <div className="grid grid-cols-3 gap-3">
            <div className="space-y-1.5">
              <Label htmlFor="req-status">Status</Label>
              <select id="req-status" className={cn(selectClass, "w-full")} value={status} onChange={(e) => setStatus(e.target.value)}>
                {STATUSES.map((s) => (
                  <option key={s.id} value={s.id}>
                    {s.label}
                  </option>
                ))}
              </select>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="req-priority">Priority</Label>
              <Input id="req-priority" type="number" value={priority} onChange={(e) => setPriority(e.target.value)} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="req-labels">Labels</Label>
              <Input id="req-labels" value={labels} onChange={(e) => setLabels(e.target.value)} placeholder="ui, api" />
            </div>
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={!title.trim() || create.isPending || patch.isPending}>
              {requirement ? "Save" : "Create"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

/** The requirement with what works on it: tasks, sessions, the pull request. */
function Detail({ projectId, id, role, onEdit, onGone }: { projectId: string; id: string; role: Role; onEdit: (r: Requirement) => void; onGone: () => void }) {
  const q = useRequirement(projectId, id);
  const patch = usePatchRequirement(projectId);
  const dispatch = useDispatchRequirement(projectId);
  const del = useDeleteRequirement(projectId);
  if (q.isLoading) return <ListSkeleton rows={3} />;
  if (q.error || !q.data) return <ErrorState error={q.error} retry={() => void q.refetch()} />;
  const { requirement: r, tasks, threads } = q.data;
  const canWork = role !== "viewer";
  const active = (r.status === "in_progress" || r.status === "in_review") && !r.flag;
  const closed = r.status === "done" || r.status === "rejected";
  return (
    <section aria-label="Requirement detail" className="space-y-4 rounded-lg border bg-card p-4">
      <div className="flex flex-wrap items-start gap-2">
        <h3 className="min-w-0 flex-1 break-words text-base font-semibold">{r.title}</h3>
        <StatusBadge status={r.status} />
      </div>
      {r.flag ? (
        <p role="alert" className="flex items-start gap-2 rounded-md border border-orange-500/40 bg-warning/10 p-2 text-sm">
          <AlertTriangle className="mt-0.5 size-4 shrink-0 text-warning" aria-hidden />
          {r.flag}
        </p>
      ) : null}
      {r.body ? <p className="whitespace-pre-wrap break-words text-sm">{r.body}</p> : <p className="text-sm text-muted-foreground">No description.</p>}
      <p className="text-xs text-muted-foreground">
        {r.source.kind === "github_issue" ? (
          <>
            From{" "}
            <a href={r.source.url} target="_blank" rel="noreferrer noopener" className="underline">
              {r.source.repo}#{r.source.number}
            </a>
            {" · "}
          </>
        ) : null}
        {r.author ? `by ${r.author}` : ""}
        {r.priority ? ` · priority ${r.priority}` : ""}
      </p>
      {r.labels?.length ? (
        <div className="flex flex-wrap gap-1">
          {r.labels.map((l) => (
            <Badge key={l} variant="secondary">
              {l}
            </Badge>
          ))}
        </div>
      ) : null}

      <div className="space-y-2">
        <h4 className="text-xs font-medium uppercase tracking-wide text-muted-foreground">Work</h4>
        {tasks.length === 0 && threads.length === 0 ? <p className="text-sm text-muted-foreground">Nothing is working on it yet.</p> : null}
        <ul className="space-y-1 text-sm">
          {tasks.map((t) => (
            <li key={t.id} className="flex flex-wrap items-center gap-2">
              <Badge variant="outline">{t.state}</Badge>
              <span className="min-w-0 break-words">{t.title}</span>
              {t.reason ? <span className="text-xs text-muted-foreground">{t.reason}</span> : null}
            </li>
          ))}
          {threads.map((t) => (
            <li key={t.id}>
              <Link to="/t/$threadId" params={{ threadId: t.id }} className="hover:underline">
                Session: {t.title || "(untitled)"}
              </Link>
            </li>
          ))}
        </ul>
        {r.pr?.number ? (
          <p className="flex items-center gap-1.5 text-sm">
            <GitPullRequest className="size-4" aria-hidden />
            {r.pr.url ? (
              <a href={r.pr.url} target="_blank" rel="noreferrer noopener" className="inline-flex items-center gap-1 hover:underline">
                Pull request #{r.pr.number} <ExternalLink className="size-3" aria-hidden />
              </a>
            ) : (
              <span>Pull request #{r.pr.number}</span>
            )}
            <Badge variant="outline">{r.pr.state}</Badge>
          </p>
        ) : null}
      </div>

      {canWork ? (
        <div className="flex flex-wrap items-center gap-2">
          <Button size="sm" disabled={closed || active || dispatch.isPending} onClick={() => dispatch.mutate(r.id)}>
            Dispatch
          </Button>
          <label className="flex items-center gap-1.5 text-sm">
            <span className="text-muted-foreground">Status</span>
            <select
              aria-label="Status"
              className={selectClass}
              value={r.status}
              onChange={(e) => patch.mutate({ id: r.id, patch: { status: e.target.value } })}
            >
              {STATUSES.map((s) => (
                <option key={s.id} value={s.id}>
                  {s.label}
                </option>
              ))}
            </select>
          </label>
          <Button size="sm" variant="outline" onClick={() => onEdit(r)}>
            <Pencil className="size-3.5" aria-hidden /> Edit
          </Button>
          {role === "owner" ? (
            <Button
              size="sm"
              variant="ghost"
              aria-label="Delete requirement"
              onClick={() => {
                if (window.confirm(`Delete "${r.title}"?`)) del.mutate(r.id, { onSuccess: onGone });
              }}
            >
              <Trash2 className="size-3.5" aria-hidden />
            </Button>
          ) : null}
        </div>
      ) : null}
    </section>
  );
}

/** Owner: dispatch a requirement as soon as it becomes ready, under a budget. */
function AutoDispatch({ projectId }: { projectId: string }) {
  const q = useRequirementSettings(projectId);
  const save = usePutRequirementSettings(projectId);
  const [on, setOn] = useState(false);
  const [usd, setUsd] = useState("");
  const [policy, setPolicy] = useState("sandboxed");
  const s = q.data;
  useEffect(() => {
    if (!s) return;
    setOn(s.autoDispatch);
    setUsd(s.budget?.maxUsd ? String(s.budget.maxUsd) : "");
    setPolicy(s.policy || "sandboxed");
  }, [s]);
  const budget = Number.parseFloat(usd);
  if (!s) return null; // the form starts from what is saved
  const valid = !on || budget > 0;
  return (
    <section aria-label="Auto-dispatch" className="space-y-2 rounded-lg border p-3 text-sm">
      <div className="flex items-center gap-2">
        <Switch id="req-auto" checked={on} onCheckedChange={setOn} aria-label="Auto-dispatch ready requirements" />
        <Label htmlFor="req-auto">Auto-dispatch ready requirements</Label>
      </div>
      <p className="text-xs text-muted-foreground">A requirement moved to Ready is handed to an agent at once, with this budget. Off by default; a budget is required.</p>
      <div className="flex flex-wrap items-end gap-3">
        <div className="space-y-1">
          <Label htmlFor="req-budget">Budget per task (USD)</Label>
          <Input id="req-budget" type="number" min="0" step="0.5" value={usd} onChange={(e) => setUsd(e.target.value)} className="w-32" />
        </div>
        <div className="space-y-1">
          <Label htmlFor="req-policy">Policy</Label>
          <select id="req-policy" className={selectClass} value={policy} onChange={(e) => setPolicy(e.target.value)}>
            <option value="supervised">Supervised</option>
            <option value="sandboxed">Sandboxed</option>
            <option value="autonomous">Autonomous</option>
          </select>
        </div>
        <Button size="sm" disabled={!valid || save.isPending} onClick={() => save.mutate({ autoDispatch: on, policy, budget: budget > 0 ? { maxUsd: budget } : undefined })}>
          Save
        </Button>
      </div>
    </section>
  );
}

export function Requirements({ projectId, role }: { projectId: string; role: Role }) {
  const list = useRequirements(projectId);
  const github = useGithub(projectId);
  const importIssues = useImportRequirements(projectId);
  const hostIssues = useImportIssues(projectId);
  const gitHosts = useGitHosts();
  const [status, setStatus] = useState<string>("all");
  const [search, setSearch] = useState("");
  // The open requirement is in the address (?req=): it is its own editor tab, and a shared link lands on it.
  const navigate = useNavigate();
  const selected = useSearch({ from: "/p/$projectId" }).req ?? "";
  const setSelected = (req: string) => void navigate({ to: "/p/$projectId", params: { projectId }, search: { tab: "requirements", req: req || undefined } });
  const [editing, setEditing] = useState<{ requirement?: Requirement } | null>(null);
  const [importLabel, setImportLabel] = useState("");
  const items = list.data ?? [];
  const counts = useMemo(() => {
    const c: Record<string, number> = {};
    for (const r of items) c[r.status] = (c[r.status] ?? 0) + 1;
    return c;
  }, [items]);
  const shown = useMemo(() => {
    const q = search.trim().toLowerCase();
    return items.filter((r) => (status === "all" || r.status === status) && (!q || r.title.toLowerCase().includes(q) || (r.body ?? "").toLowerCase().includes(q) || (r.labels ?? []).some((l) => l.toLowerCase() === q)));
  }, [items, status, search]);

  if (list.isLoading) return <ListSkeleton rows={4} />;
  if (list.error) return <ErrorState error={list.error} retry={() => void list.refetch()} />;
  if (list.data === null) return <Empty title="Requirements are not enabled">This server does not track requirements.</Empty>;
  const canWork = role !== "viewer";
  if (selected)
    return (
      <div className="space-y-3">
        <Button variant="ghost" size="sm" onClick={() => setSelected("")}>
          <ArrowLeft /> Requirements
        </Button>
        <Detail projectId={projectId} id={selected} role={role} onEdit={(r) => setEditing({ requirement: r })} onGone={() => setSelected("")} />
        <EditDialog projectId={projectId} requirement={editing?.requirement} open={editing !== null} onOpenChange={(o) => !o && setEditing(null)} />
      </div>
    );
  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-2">
        <Input aria-label="Search requirements" placeholder="Search" value={search} onChange={(e) => setSearch(e.target.value)} className="max-w-56" />
        <div className="ml-auto flex flex-wrap items-center gap-2">
          {canWork && github.data?.connected ? (
            <>
              <Input aria-label="Import label" placeholder="label (optional)" value={importLabel} onChange={(e) => setImportLabel(e.target.value)} className="w-36" />
              <Button
                variant="outline"
                size="sm"
                disabled={importIssues.isPending}
                onClick={() => importIssues.mutate(importLabel.trim())}
              >
                Import from GitHub
              </Button>
            </>
          ) : null}
          {canWork && !github.data?.connected && gitHosts.data?.hosts.some((h) => h.api) ? (
            <>
              <Input aria-label="Import label" placeholder="label (optional)" value={importLabel} onChange={(e) => setImportLabel(e.target.value)} className="w-36" />
              <Button variant="outline" size="sm" disabled={hostIssues.isPending} onClick={() => hostIssues.mutate(importLabel.trim())}>
                Import from git host
              </Button>
            </>
          ) : null}
          {canWork ? (
            <Button size="sm" onClick={() => setEditing({})}>
              <Plus className="size-4" aria-hidden /> New requirement
            </Button>
          ) : null}
        </div>
      </div>
      {importIssues.data || hostIssues.data ? (
        <p role="status" className="text-sm text-muted-foreground">
          Imported {(hostIssues.data ?? importIssues.data)!.imported} issue(s), skipped {(hostIssues.data ?? importIssues.data)!.skipped} already known.
        </p>
      ) : null}

      <div role="group" aria-label="Filter by status" className="flex flex-wrap gap-1.5">
        {[{ id: "all", label: "All", dot: "" }, ...STATUSES].map((s) => {
          const n = s.id === "all" ? items.length : (counts[s.id] ?? 0);
          return (
            <Button key={s.id} size="sm" variant={status === s.id ? "secondary" : "ghost"} aria-pressed={status === s.id} onClick={() => setStatus(s.id)} className="gap-1.5">
              {s.dot ? <span className={cn("size-2 rounded-full", s.dot)} /> : null}
              {s.label}
              <span className="tabular-nums text-muted-foreground">{n}</span>
            </Button>
          );
        })}
      </div>

      {role === "owner" ? <AutoDispatch projectId={projectId} /> : null}

      {items.length === 0 ? (
        <Empty title="No requirements yet">Write down what the project wants done{github.data?.connected ? ", or import the open issues of its repository" : ""}.</Empty>
      ) : (
        <div className="grid gap-4 md:grid-cols-[minmax(0,1fr)_minmax(0,1.2fr)]">
          <ul aria-label="Requirements" className="space-y-2">
            {shown.length === 0 ? <li className="p-4 text-sm text-muted-foreground">No requirement matches.</li> : null}
            {shown.map((r) => (
              <li key={r.id}>
                <button
                  type="button"
                  aria-current={selected === r.id}
                  onClick={() => setSelected(r.id)}
                  className={cn("block w-full rounded-lg border bg-card p-3 text-left text-sm shadow-xs transition-colors hover:bg-accent", selected === r.id && "border-primary")}
                >
                  <span className="flex items-start gap-2">
                    <span className="min-w-0 flex-1 break-words font-medium">{r.title}</span>
                    <StatusBadge status={r.status} />
                  </span>
                  <span className="mt-1.5 flex flex-wrap items-center gap-1.5 text-xs text-muted-foreground">
                    {r.flag ? (
                      <span className="inline-flex items-center gap-1 text-warning">
                        <AlertTriangle className="size-3" aria-hidden /> needs attention
                      </span>
                    ) : null}
                    {r.pr?.number ? (
                      <span className="inline-flex items-center gap-1">
                        <GitPullRequest className="size-3" aria-hidden /> #{r.pr.number}
                      </span>
                    ) : null}
                    {(r.labels ?? []).map((l) => (
                      <Badge key={l} variant="secondary">
                        {l}
                      </Badge>
                    ))}
                  </span>
                </button>
              </li>
            ))}
          </ul>
          <div className="hidden md:block">
            <p className="p-4 text-sm text-muted-foreground">Open a requirement to see what works on it. It opens in its own tab.</p>
          </div>
        </div>
      )}
      <EditDialog projectId={projectId} requirement={editing?.requirement} open={editing !== null} onOpenChange={(o) => !o && setEditing(null)} />
    </div>
  );
}
