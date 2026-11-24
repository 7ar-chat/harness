// The Roles tab of a project: the agent configurations a project (or a person) defines, the five builtin ones, and the editor
// with its version history and a preview of what a run in the role would get. Roles are called with @ in the chat composer, by
// the delegate tool and by factory tasks.
import { Copy, Eye, History, Lock, Pencil, Plus, Trash2, User as UserIcon } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { useCommands, useProjectCapabilities } from "@/api/queries";
import {
  useCreateRole, useDeleteRole, useDuplicateRole, usePreviewRole, useRestoreRole, useRoleHistory, useRoles, useUpdateRole, type RoleBody,
} from "@/api/roles";
import { useExecutors } from "@/api/executors";
import type { ExecutorView, RoleView } from "@/api/types";
import { ProjectExecutors } from "@/pages/Executors";
import { Advanced } from "@/components/app/advanced";
import { Empty, ErrorState, ListSkeleton } from "@/components/app/common";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Textarea } from "@/components/ui/textarea";
import { cn } from "@/lib/utils";

const NAME_RE = /^[a-z][a-z0-9-]{0,31}$/;

/** The first problem with a draft, mirroring the server, or undefined. */
export function validateRole(b: Pick<RoleBody, "name" | "prompt" | "toolProfile" | "permissionMode">): string | undefined {
  if (!NAME_RE.test(b.name)) return "The name is 1 to 32 characters: a lowercase letter first, then lowercase letters, digits or -.";
  if (!b.prompt.trim()) return "Write the instructions the role gives the agent.";
  if (b.toolProfile === "write" && b.permissionMode === "plan") return "A role that writes cannot run in plan mode.";
  return undefined;
}

/** "a, b\nc" as a list without blanks or repeats. */
export const parseList = (s: string): string[] => [...new Set(s.split(/[\n,]/).map((x) => x.trim()).filter(Boolean))];
const listText = (l?: string[]) => (l ?? []).join(", ");

const emptyDraft = (): RoleBody => ({
  name: "", title: "", description: "", prompt: "", model: "", reasoningEffort: "", toolProfile: "", allowTools: [], denyTools: [], skills: [], mcp: [], permissionMode: "", executor: "", enabled: true,
});

const draftOf = (r: RoleView): RoleBody => ({
  name: r.name, title: r.title ?? "", description: r.description ?? "", prompt: r.prompt, model: r.model ?? "", reasoningEffort: r.reasoningEffort ?? "", toolProfile: r.toolProfile ?? "",
  allowTools: r.allowTools ?? [], denyTools: r.denyTools ?? [], skills: r.skills ?? [], mcp: r.mcp ?? [], permissionMode: r.permissionMode ?? "", executor: r.executor ?? "", enabled: r.enabled, version: r.version,
});

const SCOPE_LABEL: Record<string, string> = { builtin: "Builtin", project: "Project", user: "Personal" };

/** Toggle chips over what exists, plus what the role already names that no longer does. */
function ChipPicker({ label, options, value, onChange, empty }: { label: string; options: string[]; value: string[]; onChange: (v: string[]) => void; empty: string }) {
  const all = [...new Set([...options, ...value])];
  return (
    <div className="space-y-1.5">
      <Label>{label}</Label>
      {all.length ? (
        <div className="flex flex-wrap gap-1.5" role="group" aria-label={label}>
          {all.map((o) => {
            const on = value.includes(o);
            return (
              <button
                key={o}
                type="button"
                aria-pressed={on}
                onClick={() => onChange(on ? value.filter((x) => x !== o) : [...value, o])}
                className={cn("rounded-full border px-2.5 py-0.5 text-xs", on ? "border-primary bg-primary/10 text-foreground" : "text-muted-foreground hover:bg-accent", !options.includes(o) && "border-dashed")}
              >
                {o}
              </button>
            );
          })}
        </div>
      ) : (
        <p className="text-xs text-muted-foreground">{empty}</p>
      )}
    </div>
  );
}

function RoleEditor({ pid, role, create, readOnly, onClose, onDuplicate }: { pid?: string; role?: RoleView; create?: boolean; readOnly?: boolean; onClose: () => void; onDuplicate?: (r: RoleView) => void }) {
  const [d, setD] = useState<RoleBody>(() => (role ? draftOf(role) : emptyDraft()));
  const set = (p: Partial<RoleBody>) => setD((x) => ({ ...x, ...p }));
  const [allow, setAllow] = useState(listText(role?.allowTools));
  const [deny, setDeny] = useState(listText(role?.denyTools));
  const skillOptions = (useCommands().data ?? []).filter((c) => c.source === "skill").map((c) => c.name);
  const mcpOptions = (useProjectCapabilities(pid ?? "").data ?? []).filter((c) => c.enabled).map((c) => c.name);
  // The executors in reach; one the role names that is gone is kept in the list so the editor does not lose it silently.
  const executorList = (useExecutors(pid).data ?? []).filter((e) => e.enabled);
  const executorOptions = role?.executor && !executorList.some((e) => e.name === role.executor) ? [...executorList, { name: role.executor, kind: "custom" } as ExecutorView] : executorList;
  const create_ = useCreateRole(pid);
  const update = useUpdateRole(pid);
  const preview = usePreviewRole(pid);
  const restore = useRestoreRole(pid);
  const history = useRoleHistory(pid, role && !create && !role.builtin ? role.name : undefined, role?.version);
  const [showHistory, setShowHistory] = useState(false);
  // An empty executor is left out of the body: a PUT replaces the role, so absent means the internal loop.
  const body = useMemo<RoleBody>(() => ({ ...d, executor: d.executor || undefined, allowTools: parseList(allow), denyTools: parseList(deny) }), [d, allow, deny]);
  const problem = readOnly ? undefined : validateRole(body);
  // Advanced opens by itself when it holds a non-default value, or when the problem with the draft is one of its fields.
  const advancedSet = !!(d.model || d.reasoningEffort || d.toolProfile || d.permissionMode || body.allowTools.length || body.denyTools.length || d.skills.length || d.mcp.length);
  const advancedProblem = !!problem && validateRole({ ...body, name: "ok", prompt: "ok" }) === problem;
  const busy = create_.isPending || update.isPending;
  const editing = !create && !!role;

  const save = () => {
    const done = { onSuccess: onClose };
    if (editing) update.mutate(body, done);
    else create_.mutate(body, done);
  };

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{create ? "New role" : readOnly ? `@${role?.name}` : `Edit @${role?.name}`}</DialogTitle>
          <DialogDescription>
            {readOnly ? "A builtin role cannot be edited. Copy it into the project to change it: the copy replaces the builtin of the same name here." : "A role is an agent configuration you call with @ in the chat, from the delegate tool or on a task."}
          </DialogDescription>
        </DialogHeader>
        {readOnly ? null : <p className="text-sm text-muted-foreground">Most roles only need a name and instructions.</p>}
        <fieldset disabled={readOnly} className="space-y-4">
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="space-y-1.5">
              <Label htmlFor="role-name">Name</Label>
              <Input id="role-name" value={d.name} disabled={editing || readOnly} placeholder="docs-writer" onChange={(e) => set({ name: e.target.value.toLowerCase() })} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="role-title">Title</Label>
              <Input id="role-title" value={d.title} placeholder="Docs writer (optional)" onChange={(e) => set({ title: e.target.value })} />
            </div>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="role-description">Description</Label>
            <Input id="role-description" value={d.description} placeholder="What this role does (shown to the delegating agent)" onChange={(e) => set({ description: e.target.value })} />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="role-prompt">Instructions</Label>
            <Textarea id="role-prompt" rows={8} value={d.prompt} placeholder="You are … Do … Never …" onChange={(e) => set({ prompt: e.target.value })} />
          </div>
          <div className="space-y-1.5">
            <Label>Executor</Label>
            <p className="text-xs text-muted-foreground">Who does the work.</p>
            <Select value={d.executor || "internal"} onValueChange={(v) => set({ executor: v === "internal" ? "" : v })} disabled={readOnly}>
              <SelectTrigger aria-label="Executor"><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem value="internal">7ar harness (built-in loop) · Recommended</SelectItem>
                {executorOptions.map((e) => (
                  <SelectItem key={e.name} value={e.name}>{e.title || e.name}{e.kind === "custom" ? " (project)" : ""}</SelectItem>
                ))}
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground">
              {d.executor
                ? "An external CLI runs on its own loop in the sandbox, under the task's policy. It does not use the project's memory, and its spend is counted only if the CLI reports it. Each person sets their own API key in Account."
                : "Recommended. The 7ar harness codes in the sandbox with the project's memory and instructions, loop guards, budgets that count every token, approvals in the UI, and queue, steer and resume. It needs no setup."}
            </p>
          </div>
          <div className="flex items-center gap-2">
            <Switch id="role-enabled" checked={d.enabled} onCheckedChange={(enabled) => set({ enabled })} aria-label="Enabled" />
            <Label htmlFor="role-enabled">Enabled</Label>
            <span className="text-xs text-muted-foreground">A disabled role is not offered and cannot be called.</span>
          </div>
        </fieldset>

        <Advanced storageKey="7ar.advanced.role" nonDefault={advancedSet} invalid={advancedProblem} hint="Model, tools, skills, history">
          <fieldset disabled={readOnly} className="space-y-4">
            <div className="grid gap-3 sm:grid-cols-2">
              <div className="space-y-1.5">
                <Label htmlFor="role-model">Model</Label>
                <Input id="role-model" value={d.model} placeholder="the default model" onChange={(e) => set({ model: e.target.value })} />
              </div>
              <div className="space-y-1.5">
                <Label>Reasoning effort</Label>
                <Select value={d.reasoningEffort || "default"} onValueChange={(v) => set({ reasoningEffort: v === "default" ? "" : v })} disabled={readOnly}>
                  <SelectTrigger aria-label="Reasoning effort"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="default">Default</SelectItem>
                    <SelectItem value="low">Low</SelectItem>
                    <SelectItem value="medium">Medium</SelectItem>
                    <SelectItem value="high">High</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1.5">
                <Label>Tool profile</Label>
                <Select value={d.toolProfile || "inherit"} onValueChange={(v) => set({ toolProfile: v === "inherit" ? "" : v })} disabled={readOnly}>
                  <SelectTrigger aria-label="Tool profile"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="inherit">Same tools as the run</SelectItem>
                    <SelectItem value="read-only">Read-only (nothing is changed)</SelectItem>
                    <SelectItem value="write">Read and write</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1.5">
                <Label>Permission mode</Label>
                <Select value={d.permissionMode || "normal"} onValueChange={(v) => set({ permissionMode: v === "normal" ? "" : v })} disabled={readOnly}>
                  <SelectTrigger aria-label="Permission mode"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="normal">Normal</SelectItem>
                    <SelectItem value="plan">Plan (investigate, then propose)</SelectItem>
                    <SelectItem value="ask">Ask before risky tool calls</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            </div>
            <div className="grid gap-3 sm:grid-cols-2">
              <div className="space-y-1.5">
                <Label htmlFor="role-allow">Allowed tools</Label>
                <Textarea id="role-allow" rows={2} value={allow} placeholder="empty: all of them. read_file, search_*" onChange={(e) => setAllow(e.target.value)} />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="role-deny">Denied tools</Label>
                <Textarea id="role-deny" rows={2} value={deny} placeholder="execute_command, github__*" onChange={(e) => setDeny(e.target.value)} />
              </div>
            </div>
            <ChipPicker label="Skills to load" options={skillOptions} value={d.skills} onChange={(skills) => set({ skills })} empty="No skills are installed." />
            <ChipPicker label="MCP servers" options={mcpOptions} value={d.mcp} onChange={(mcp) => set({ mcp })} empty="No MCP server is on for this project: the role keeps the tools the run has." />
          </fieldset>
          {editing && !role?.builtin ? (
            <div>
              <Button type="button" variant="outline" size="sm" aria-pressed={showHistory} onClick={() => setShowHistory((s) => !s)}>
                <History /> History
              </Button>
            </div>
          ) : null}
          {showHistory ? (
            <div className="space-y-1 rounded-md border p-2" aria-label="Version history">
              {history.isLoading ? <ListSkeleton rows={2} /> : null}
              {history.data?.length === 0 ? <p className="p-1 text-xs text-muted-foreground">No earlier versions: the role has not been changed since it was created.</p> : null}
              {history.data?.map((v) => (
                <div key={v.version} className="flex items-center gap-2 text-sm">
                  <span className="font-medium">v{v.version}</span>
                  <span className="min-w-0 flex-1 truncate text-xs text-muted-foreground">
                    {v.changedBy ? `${v.changedBy}, ` : ""}
                    {new Date(v.at).toLocaleString()} · {v.role.prompt.slice(0, 60)}
                  </span>
                  <Button size="sm" variant="outline" disabled={restore.isPending || readOnly} onClick={() => role && restore.mutate({ name: role.name, version: v.version }, { onSuccess: onClose })}>
                    Restore
                  </Button>
                </div>
              ))}
            </div>
          ) : null}
        </Advanced>

        {preview.data ? (
          <div className="space-y-2 rounded-md border bg-muted/40 p-3 text-sm" aria-label="Preview">
            <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
              <Badge variant="secondary">{preview.data.mode === "plan" ? "plan mode" : "build mode"}</Badge>
              {preview.data.model ? <span>model {preview.data.model}</span> : null}
              {preview.data.reasoningEffort ? <span>effort {preview.data.reasoningEffort}</span> : null}
              {preview.data.skills?.length ? <span>skills {preview.data.skills.join(", ")}</span> : null}
            </div>
            <pre className="max-h-60 overflow-auto whitespace-pre-wrap text-xs">{preview.data.prompt}</pre>
            {preview.data.notes?.length ? (
              <ul className="list-disc pl-4 text-xs text-muted-foreground">
                {preview.data.notes.map((n) => (
                  <li key={n}>{n}</li>
                ))}
              </ul>
            ) : null}
          </div>
        ) : null}

        {problem && (d.name || d.prompt) ? <p role="alert" className="text-xs text-destructive">{problem}</p> : null}
        <DialogFooter className="flex-wrap gap-2 sm:justify-between">
          <div className="flex gap-2">
            <Button type="button" variant="outline" size="sm" disabled={(!!problem && !readOnly) || preview.isPending} onClick={() => preview.mutate({ ...body, name: body.name || "preview" })}>
              <Eye /> Preview
            </Button>
            {readOnly && role && onDuplicate ? (
              <Button type="button" size="sm" onClick={() => onDuplicate(role)}>
                <Copy /> Copy to the project
              </Button>
            ) : null}
          </div>
          <div className="flex gap-2">
            <Button type="button" variant="ghost" onClick={onClose}>
              {readOnly ? "Close" : "Cancel"}
            </Button>
            {readOnly ? null : (
              <Button type="button" disabled={!!problem || busy} onClick={save}>
                {create ? "Create" : "Save"}
              </Button>
            )}
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function RoleRow({ r, onOpen, onEdit, onDuplicate, onDelete, onToggle, canEdit }: { r: RoleView; onOpen: () => void; onEdit: () => void; onDuplicate: () => void; onDelete: () => void; onToggle: (on: boolean) => void; canEdit: boolean }) {
  const tools = [r.executor ? `runs on ${r.executor}` : "", r.toolProfile === "read-only" ? "read-only" : "", r.allowTools?.length ? `${r.allowTools.length} allowed tools` : "", r.denyTools?.length ? `${r.denyTools.length} denied` : "", r.mcp?.length ? `MCP ${r.mcp.join(", ")}` : ""].filter(Boolean);
  return (
    <li className={cn("flex flex-wrap items-center gap-3 p-3", !r.enabled && "opacity-60")}>
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-2">
          <button type="button" className="font-medium hover:underline" onClick={onOpen}>
            {r.title || r.name}
          </button>
          <code className="text-xs text-muted-foreground">@{r.name}</code>
          <Badge variant={r.scope === "builtin" ? "secondary" : "outline"} className="gap-1">
            {r.scope === "builtin" ? <Lock className="size-3" /> : r.scope === "user" ? <UserIcon className="size-3" /> : null}
            {SCOPE_LABEL[r.scope] ?? r.scope}
          </Badge>
          {r.overrides ? <Badge variant="outline">overrides the builtin</Badge> : null}
          {!r.enabled ? <Badge variant="outline">disabled</Badge> : null}
          {r.scope !== "builtin" ? <span className="text-xs text-muted-foreground">v{r.version}</span> : null}
        </div>
        {r.description ? <p className="truncate text-sm text-muted-foreground">{r.description}</p> : null}
        <p className="text-xs text-muted-foreground">
          {[r.model ? `model ${r.model}` : "default model", ...tools, r.skills?.length ? `skills ${r.skills.join(", ")}` : ""].filter(Boolean).join(" · ")}
        </p>
      </div>
      <div className="flex items-center gap-1">
        {r.editable ? <Switch checked={r.enabled} onCheckedChange={onToggle} aria-label={`Enable ${r.name}`} /> : null}
        {r.editable ? (
          <Button size="icon" variant="ghost" aria-label={`Edit ${r.name}`} onClick={onEdit}>
            <Pencil />
          </Button>
        ) : (
          <Button size="icon" variant="ghost" aria-label={`View ${r.name}`} onClick={onOpen}>
            <Eye />
          </Button>
        )}
        {canEdit ? (
          <Button size="icon" variant="ghost" aria-label={`Copy ${r.name}`} title={r.scope === "builtin" ? "Copy into the project to change it" : "Duplicate"} onClick={onDuplicate}>
            <Copy />
          </Button>
        ) : null}
        {r.editable ? (
          <Button size="icon" variant="ghost" aria-label={`Delete ${r.name}`} title={r.overrides ? "Delete the copy: the builtin comes back" : "Delete"} onClick={onDelete}>
            <Trash2 />
          </Button>
        ) : null}
      </div>
    </li>
  );
}

/** The project's roles; `projectId` undefined shows the caller's personal ones. */
export function RolesPanel({ projectId, canEdit }: { projectId?: string; canEdit: boolean }) {
  const roles = useRoles(projectId);
  const update = useUpdateRole(projectId);
  const duplicate = useDuplicateRole(projectId);
  const del = useDeleteRole(projectId);
  const [editing, setEditing] = useState<{ role?: RoleView; create?: boolean; readOnly?: boolean } | null>(null);
  const [removing, setRemoving] = useState<RoleView | null>(null);
  // The editor works on a snapshot: close it when the role it shows was removed.
  useEffect(() => {
    if (editing?.role && roles.data && !roles.data.some((r) => r.name === editing.role?.name && r.scope === editing.role?.scope)) setEditing(null);
  }, [roles.data, editing]);

  if (roles.isLoading) return <ListSkeleton />;
  if (roles.error) return <ErrorState error={roles.error} retry={() => void roles.refetch()} />;
  if (roles.data === null) return <Empty title="Roles are not available">This server has no role registry.</Empty>;
  const list = roles.data ?? [];
  const copy = (r: RoleView) => duplicate.mutate({ name: r.name }, { onSuccess: () => setEditing(null) });

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        {canEdit ? (
          <Button onClick={() => setEditing({ create: true })}>
            <Plus /> New role
          </Button>
        ) : null}
      </div>
      <ul className="divide-y rounded-lg border" aria-label="Roles">
        {list.map((r) => (
          <RoleRow
            key={`${r.scope}:${r.name}`}
            r={r}
            canEdit={canEdit}
            onOpen={() => setEditing({ role: r, readOnly: !r.editable })}
            onEdit={() => setEditing({ role: r })}
            onDuplicate={() => copy(r)}
            onDelete={() => setRemoving(r)}
            onToggle={(enabled) => update.mutate({ ...draftOf(r), enabled })}
          />
        ))}
      </ul>
      {projectId ? <ProjectExecutors projectId={projectId} canEdit={canEdit} /> : null}
      {editing ? (
        <RoleEditor pid={projectId} role={editing.role} create={editing.create} readOnly={editing.readOnly} onClose={() => setEditing(null)} onDuplicate={copy} />
      ) : null}
      <AlertDialog open={!!removing} onOpenChange={(o) => !o && setRemoving(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete @{removing?.name}?</AlertDialogTitle>
            <AlertDialogDescription>
              {removing?.overrides ? "The builtin role of the same name comes back." : "Chats, tasks and delegations that name it will no longer find it."} Its version history is deleted with it.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={() => removing && del.mutate(removing.name)}>Delete</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

/** The caller's own roles, at /roles: the same editor as a project's Roles tab, for roles only they can use. */
export function RolesPage() {
  return (
    <ScrollArea className="h-full">
      <div className="w-full max-w-5xl space-y-4 p-4 md:p-6">
        <h1 className="text-xl font-semibold">My roles</h1>
        <p className="text-sm text-muted-foreground">A role is a named agent configuration: instructions, model, tools and skills. These are yours alone: type @ in a session to run as one. A project role of the same name wins inside that project.</p>
        <RolesPanel canEdit />
      </div>
    </ScrollArea>
  );
}
