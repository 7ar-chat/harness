// External coding-agent executors: a user's sealed credentials for them (Settings), and a project's own definitions (Roles tab).
// An executor is a CLI agent (Claude Code, Codex, OpenCode, or a command the project defines) that runs inside the sandbox in place
// of the internal agent loop; a role that names one makes the delegate tool and factory tasks of that role use it. API keys are
// write-only: the server stores them sealed and never sends them back.
import { HelpButton } from "@/components/app/help";
import { KeyRound, Pencil, Plus, Terminal, Trash2 } from "lucide-react";
import { useState } from "react";
import {
  useDeleteExecutor, useDeleteExecutorCredentials, useExecutors, usePutExecutorCredentials, useSaveExecutor, type ExecutorBody,
} from "@/api/executors";
import type { ExecutorView } from "@/api/types";
import { ErrorState, ListSkeleton } from "@/components/app/common";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { cn } from "@/lib/utils";

const NAME_RE = /^[a-z][a-z0-9-]{0,23}$/;
const ENV_RE = /^[A-Z][A-Z0-9_]{0,63}$/;

/** The first problem with a draft, mirroring the server, or undefined. */
export function validateExecutor(b: Pick<ExecutorBody, "name" | "command" | "binary" | "credentialEnv">): string | undefined {
  if (!NAME_RE.test(b.name)) return "The name is 1 to 24 characters: a lowercase letter first, then lowercase letters, digits or -.";
  if (!b.command.includes("{task}")) return "The command must contain {task}, where the task goes.";
  if (!b.binary.trim()) return "Name the program the command runs, so the sandbox can be checked for it.";
  const bad = b.credentialEnv.find((n) => !ENV_RE.test(n));
  if (bad) return `${bad} is not an upper case variable name such as MY_API_KEY.`;
  return undefined;
}

const parseEnv = (s: string) => [...new Set(s.split(/[\s,]+/).map((x) => x.trim()).filter(Boolean))];

/** One executor's credential form: a field per variable the executor declares. Values are never shown again. */
function CredentialsForm({ ex, project, onDone }: { ex: ExecutorView; project?: string; onDone: () => void }) {
  const put = usePutExecutorCredentials();
  const del = useDeleteExecutorCredentials();
  const [vals, setVals] = useState<Record<string, string>>({});
  const names = ex.credentialEnv ?? [];
  const changed = Object.fromEntries(Object.entries(vals).filter(([, v]) => v.trim() !== ""));
  return (
    <form
      className="space-y-3"
      onSubmit={(e) => {
        e.preventDefault();
        put.mutate({ name: ex.name, env: changed, project }, { onSuccess: onDone });
      }}
    >
      {names.length === 0 ? <p className="text-sm text-muted-foreground">This executor takes no credentials: it uses what the sandbox has.</p> : null}
      {names.map((n) => (
        <div key={n} className="space-y-1">
          <Label htmlFor={`cred-${ex.name}-${n}`}>{n}</Label>
          <Input
            id={`cred-${ex.name}-${n}`}
            type={n.endsWith("_URL") ? "url" : "password"}
            autoComplete="off"
            value={vals[n] ?? ""}
            placeholder={ex.configured ? "set: leave empty to keep it" : n.endsWith("_URL") ? "optional: the provider's default" : "not set"}
            onChange={(e) => setVals((x) => ({ ...x, [n]: e.target.value }))}
          />
        </div>
      ))}
      <p className="text-xs text-muted-foreground">Sealed on the server and handed only to the process of a run, never to the model or to disk. It cannot be read back.</p>
      <div className="flex flex-wrap gap-2">
        <Button type="submit" size="sm" disabled={put.isPending || Object.keys(changed).length === 0}>
          Save
        </Button>
        {ex.configured ? (
          <Button type="button" size="sm" variant="outline" disabled={del.isPending} onClick={() => del.mutate(ex.name, { onSuccess: onDone })}>
            Remove my credentials
          </Button>
        ) : null}
      </div>
    </form>
  );
}

const AVAILABILITY: Record<string, { label: string; title: string; variant: "secondary" | "outline" }> = {
  ready: { label: "ready", title: "The CLI is in the default sandbox image", variant: "secondary" },
  install: { label: "will be installed on first use", title: "Not in the sandbox image: a run installs it first", variant: "outline" },
  missing: { label: "not in the sandbox image", title: "Ask an admin to add it to the sandbox image", variant: "outline" },
  unknown: { label: "not checked", title: "No sandbox to check, or the check has not answered yet", variant: "outline" },
};

function AvailabilityBadge({ value }: { value?: string }) {
  const a = AVAILABILITY[value ?? "unknown"] ?? AVAILABILITY.unknown!;
  return (
    <Badge variant={a.variant} title={a.title} data-testid="executor-availability">
      {a.label}
    </Badge>
  );
}

function ExecutorLine({ ex, project, actions }: { ex: ExecutorView; project?: string; actions?: React.ReactNode }) {
  const [open, setOpen] = useState(false);
  return (
    <li className={cn("space-y-3 p-3", !ex.enabled && "opacity-60")}>
      <div className="flex flex-wrap items-center gap-2">
        <Terminal className="size-4 text-muted-foreground" aria-hidden />
        <span className="font-medium">{ex.title || ex.name}</span>
        <code className="text-xs text-muted-foreground">{ex.name}</code>
        <Badge variant={ex.kind === "builtin" ? "secondary" : "outline"}>{ex.kind === "builtin" ? "Builtin" : "Project"}</Badge>
        {!ex.enabled ? <Badge variant="outline">disabled</Badge> : null}
        {ex.configured ? <Badge variant="secondary">credentials set</Badge> : null}
        <AvailabilityBadge value={ex.availability} />
        <div className="ml-auto flex items-center gap-1">
          {ex.credentialEnv?.length ? (
            <Button size="sm" variant="outline" aria-expanded={open} aria-label={`${ex.configured ? "Change" : "Set up"} credentials for ${ex.name}`} onClick={() => setOpen((o) => !o)}>
              <KeyRound /> {ex.configured ? "Change" : "Set up"}
            </Button>
          ) : null}
          {actions}
        </div>
      </div>
      {ex.description ? <p className="text-sm text-muted-foreground">{ex.description}</p> : null}
      {ex.credentialEnv?.length ? (
        <p className="text-xs text-muted-foreground">
          Reads <code>{ex.credentialEnv.join(", ")}</code> from your Account{ex.installCommand ? <>; installed on first use with <code>{ex.installCommand}</code></> : null}.
        </p>
      ) : null}
      {open ? <CredentialsForm ex={ex} project={project} onDone={() => setOpen(false)} /> : null}
    </li>
  );
}

/** Settings: the caller's credentials for the builtin executors. */
export function ExecutorsCard() {
  const list = useExecutors();
  if (list.data === null) return null; // this server has no executors
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <Terminal className="size-4" /> Executors
        </CardTitle>
        <CardDescription>Optional external coding agents your tasks and roles can run in the sandbox. The 7ar harness is recommended and needs no key here. Your API keys stay sealed on the server.</CardDescription>
      </CardHeader>
      <CardContent>
        {list.isLoading ? <ListSkeleton rows={3} /> : null}
        {list.error ? <ErrorState error={list.error} retry={() => void list.refetch()} /> : null}
        <ul className="divide-y rounded-lg border" aria-label="Executors">
          {(list.data ?? []).map((ex) => (
            <ExecutorLine key={ex.name} ex={ex} />
          ))}
        </ul>
      </CardContent>
    </Card>
  );
}

const emptyDraft = (): ExecutorBody => ({ name: "", title: "", description: "", command: "", binary: "", installCommand: "", credentialEnv: [], parser: "", modeFlags: {}, enabled: true });
const draftOf = (e: ExecutorView): ExecutorBody => ({
  name: e.name, title: e.title ?? "", description: e.description ?? "", command: e.command, binary: e.binary, installCommand: e.installCommand ?? "",
  credentialEnv: e.credentialEnv ?? [], parser: e.parser ?? "", modeFlags: e.modeFlags ?? {}, enabled: e.enabled,
});

function ExecutorEditor({ pid, ex, onClose }: { pid: string; ex?: ExecutorView; onClose: () => void }) {
  const [d, setD] = useState<ExecutorBody>(() => (ex ? draftOf(ex) : emptyDraft()));
  const [env, setEnv] = useState((ex?.credentialEnv ?? []).join(", "));
  const save = useSaveExecutor(pid);
  const set = (p: Partial<ExecutorBody>) => setD((x) => ({ ...x, ...p }));
  const body = { ...d, credentialEnv: parseEnv(env) };
  const problem = validateExecutor(body);
  const flag = (level: string) => body.modeFlags[level] ?? "";
  const setFlag = (level: string, v: string) => set({ modeFlags: { ...d.modeFlags, [level]: v } });
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{ex ? `Edit ${ex.name}` : "New executor"}</DialogTitle>
          <DialogDescription>A command that runs a coding agent in the sandbox. The task reaches it in the environment, never in the command line.</DialogDescription>
        </DialogHeader>
        <div className="space-y-3">
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="space-y-1">
              <Label htmlFor="ex-name">Name</Label>
              <Input id="ex-name" value={d.name} disabled={!!ex} placeholder="my-agent" onChange={(e) => set({ name: e.target.value.toLowerCase() })} />
            </div>
            <div className="space-y-1">
              <Label htmlFor="ex-title">Title</Label>
              <Input id="ex-title" value={d.title} onChange={(e) => set({ title: e.target.value })} />
            </div>
          </div>
          <div className="space-y-1">
            <Label htmlFor="ex-desc">Description</Label>
            <Input id="ex-desc" value={d.description} onChange={(e) => set({ description: e.target.value })} />
          </div>
          <div className="space-y-1">
            <Label htmlFor="ex-command">Command</Label>
            <Textarea id="ex-command" rows={3} value={d.command} placeholder="mycli run --yes {mode} {task}" onChange={(e) => set({ command: e.target.value })} />
            <p className="text-xs text-muted-foreground">
              <code>{"{task}"}</code> is the task, <code>{"{mode}"}</code> the flags of the level below.
            </p>
          </div>
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="space-y-1">
              <Label htmlFor="ex-binary">Program</Label>
              <Input id="ex-binary" value={d.binary} placeholder="mycli" onChange={(e) => set({ binary: e.target.value })} />
            </div>
            <div className="space-y-1">
              <Label htmlFor="ex-install">Install command</Label>
              <Input id="ex-install" value={d.installCommand} placeholder="npm install -g mycli (optional)" onChange={(e) => set({ installCommand: e.target.value })} />
            </div>
          </div>
          <div className="grid gap-3 sm:grid-cols-3">
            {(["read-only", "edit", "full"] as const).map((l) => (
              <div key={l} className="space-y-1">
                <Label htmlFor={`ex-flags-${l}`}>Flags: {l}</Label>
                <Input id={`ex-flags-${l}`} value={flag(l)} placeholder={l === "read-only" ? "empty: cannot run read-only" : ""} onChange={(e) => setFlag(l, e.target.value)} />
              </div>
            ))}
          </div>
          <div className="space-y-1">
            <Label htmlFor="ex-env">Credential variables</Label>
            <Input id="ex-env" value={env} placeholder="MYCLI_API_KEY, MYCLI_BASE_URL" onChange={(e) => setEnv(e.target.value)} />
            <p className="text-xs text-muted-foreground">Each member sets their own values in Account; they are sealed per person.</p>
          </div>
          <div className="space-y-1">
            <Label htmlFor="ex-parser">Output</Label>
            <select id="ex-parser" className="h-9 w-full rounded-md border bg-transparent px-2 text-sm" value={d.parser} onChange={(e) => set({ parser: e.target.value })}>
              <option value="">Plain text</option>
              <option value="claude-stream-json">Claude Code stream-json</option>
              <option value="codex-jsonl">Codex JSONL</option>
            </select>
          </div>
          <div className="flex items-center gap-2">
            <Switch id="ex-enabled" checked={d.enabled} onCheckedChange={(enabled) => set({ enabled })} aria-label="Enabled" />
            <Label htmlFor="ex-enabled">Enabled</Label>
          </div>
        </div>
        {problem && (d.name || d.command) ? <p role="alert" className="text-xs text-destructive">{problem}</p> : null}
        <DialogFooter>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button disabled={!!problem || save.isPending} onClick={() => save.mutate({ body, update: !!ex }, { onSuccess: onClose })}>
            {ex ? "Save" : "Create"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/** The Roles tab's section: the executors a project can use, and, for the owner, its own definitions. */
export function ProjectExecutors({ projectId, canEdit }: { projectId: string; canEdit: boolean }) {
  const list = useExecutors(projectId);
  const del = useDeleteExecutor(projectId);
  const [editing, setEditing] = useState<{ ex?: ExecutorView } | null>(null);
  const [removing, setRemoving] = useState<string | null>(null);
  if (list.data === null || list.isLoading) return null;
  return (
    <section className="space-y-3" aria-label="Executors">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <div className="flex items-center gap-1.5">
            <h2 className="text-base font-semibold">Executors</h2>
            <HelpButton slug="executors" />
          </div>
          <p className="max-w-2xl text-sm text-muted-foreground">
            The 7ar harness is the recommended way to run a role and needs no setup. These are optional external coding agents that run in the sandbox: give a role an executor and the delegate tool and tasks of that role use it, under the task&apos;s policy.
          </p>
        </div>
        {canEdit ? (
          <Button variant="outline" onClick={() => setEditing({})}>
            <Plus /> New executor
          </Button>
        ) : null}
      </div>
      {list.error ? <ErrorState error={list.error} retry={() => void list.refetch()} /> : null}
      <ul className="divide-y rounded-lg border" aria-label="Project executors">
        {(list.data ?? []).map((ex) => (
          <ExecutorLine
            key={ex.name}
            ex={ex}
            project={projectId}
            actions={
              ex.editable ? (
                <>
                  <Button size="icon" variant="ghost" aria-label={`Edit ${ex.name}`} onClick={() => setEditing({ ex })}>
                    <Pencil />
                  </Button>
                  <Button size="icon" variant="ghost" aria-label={`Delete ${ex.name}`} onClick={() => setRemoving(ex.name)}>
                    <Trash2 />
                  </Button>
                </>
              ) : null
            }
          />
        ))}
      </ul>
      {editing ? <ExecutorEditor pid={projectId} ex={editing.ex} onClose={() => setEditing(null)} /> : null}
      <AlertDialog open={!!removing} onOpenChange={(o) => !o && setRemoving(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete {removing}?</AlertDialogTitle>
            <AlertDialogDescription>Roles that name it will no longer find it.</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={() => removing && del.mutate(removing)}>Delete</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </section>
  );
}
