import { Link, useNavigate, useParams, useSearch } from "@tanstack/react-router";
import { formatDistanceToNowStrict } from "date-fns";
import { ExternalLink, GitBranch, Plus, RefreshCw } from "lucide-react";
import { useEffect, useState } from "react";
import { isOwner, canWrite, downloadText, ApiError } from "@/api/client";
import {
  useAudit, useDeleteProject, useGithub, useImportAgents, useMemoryStats, toastError, useInvite, useInvites, useMe, useMembers, useOverview, useProject, useRecheckGithub, useRemoveMember, useRevokeInvite, useSetMember, useUpdateProject,
} from "@/api/queries";
import type { Role } from "@/api/types";
import { Empty, ErrorState, ListSkeleton, RoleBadge, StatusDot, UserAvatar } from "@/components/app/common";
import { useDialogs } from "@/components/app/dialogs";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { useGithubConnector } from "@/components/app/github-import";
import { toast } from "sonner";
import { Download, FileText } from "lucide-react";
import { MemoryManager } from "./Memory";

const ago = (iso: string) => `${formatDistanceToNowStrict(new Date(iso))} ago`;

function RoleSelect({ value, onChange, disabled }: { value: Role; onChange: (r: Role) => void; disabled?: boolean }) {
  return (
    <Select value={value} onValueChange={(v) => onChange(v as Role)} disabled={disabled}>
      <SelectTrigger size="sm" className="w-28" aria-label="Role">
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        <SelectItem value="owner">Owner</SelectItem>
        <SelectItem value="member">Member</SelectItem>
        <SelectItem value="viewer">Viewer</SelectItem>
      </SelectContent>
    </Select>
  );
}

function Members({ pid, owner, self }: { pid: string; owner: boolean; self: string }) {
  const members = useMembers(pid);
  const invites = useInvites(pid, owner);
  const setMember = useSetMember(pid);
  const remove = useRemoveMember(pid);
  const invite = useInvite(pid);
  const revoke = useRevokeInvite(pid);
  const navigate = useNavigate();
  const [email, setEmail] = useState("");
  const [role, setRole] = useState<Role>("member");
  if (members.isLoading) return <ListSkeleton />;
  if (members.error) return <ErrorState error={members.error} retry={() => void members.refetch()} />;
  return (
    <div className="space-y-6">
      <ul className="divide-y rounded-lg border">
        {members.data?.map((m) => (
          <li key={m.login} className="flex items-center gap-3 p-3">
            <UserAvatar login={m.login} name={m.name} url={m.avatarUrl} className="size-8" />
            <div className="min-w-0 flex-1">
              <div className="truncate text-sm font-medium">{m.name || m.login}{m.login === self ? " (you)" : ""}</div>
              <div className="truncate text-xs text-muted-foreground">{m.email || m.login}</div>
            </div>
            {owner ? <RoleSelect value={m.role} onChange={(r) => setMember.mutate({ login: m.login, role: r })} /> : <RoleBadge role={m.role} />}
            {owner || m.login === self ? (
              <Button
                variant="ghost"
                size="sm"
                onClick={() => remove.mutate(m.login, { onSuccess: () => m.login === self && void navigate({ to: "/" }) })}
              >
                {m.login === self ? "Leave" : "Remove"}
              </Button>
            ) : null}
          </li>
        ))}
      </ul>
      {owner ? (
        <div className="space-y-3">
          <h3 className="text-sm font-medium">Invite by email</h3>
          <form
            className="flex flex-wrap gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              invite.mutate({ email: email.trim(), role }, { onSuccess: () => setEmail("") });
            }}
          >
            <Input type="email" required value={email} onChange={(e) => setEmail(e.target.value)} placeholder="teammate@example.com" className="min-w-52 flex-1" aria-label="Email" />
            <RoleSelect value={role} onChange={setRole} />
            <Button type="submit" disabled={invite.isPending}>
              <Plus /> Invite
            </Button>
          </form>
          {invites.data?.length ? (
            <ul className="divide-y rounded-lg border">
              {invites.data.map((i) => (
                <li key={i.email} className="flex items-center gap-3 p-3 text-sm">
                  <span className="flex-1 truncate">{i.email}</span>
                  <Badge variant="outline">pending</Badge>
                  <RoleBadge role={i.role} />
                  <Button variant="ghost" size="sm" onClick={() => revoke.mutate(i.email)}>
                    Revoke
                  </Button>
                </li>
              ))}
            </ul>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

function Github({ pid, owner }: { pid: string; owner: boolean }) {
  const gh = useGithub(pid);
  const recheck = useRecheckGithub(pid);
  if (gh.isLoading || !gh.data) return gh.isLoading ? null : <p className="text-sm text-muted-foreground">GitHub integration is not available on this server.</p>;
  const g = recheck.data ?? gh.data;
  return (
    <div className="space-y-2 rounded-lg border p-4">
      <div className="flex items-center gap-2 text-sm font-medium">
        GitHub
        <Badge variant={g.connected ? "default" : "secondary"}>{g.connected ? "Connected" : g.configured ? "Not connected" : "Not configured"}</Badge>
      </div>
      {g.repo ? <div className="text-sm text-muted-foreground">{g.repo}</div> : null}
      {g.message ? <p className="text-sm text-muted-foreground">{g.message}</p> : null}
      {owner && g.configured && !g.connected ? (
        <div className="flex flex-wrap gap-2">
          {g.installUrl ? (
            <Button asChild>
              <a href={g.installUrl} target="_blank" rel="noreferrer">
                <ExternalLink /> Install GitHub App
              </a>
            </Button>
          ) : null}
          <Button variant="outline" onClick={() => recheck.mutate()} disabled={recheck.isPending}>
            <RefreshCw /> Check again
          </Button>
        </div>
      ) : null}
    </div>
  );
}

function Settings({ pid }: { pid: string }) {
  const { data: p } = useProject(pid);
  const update = useUpdateProject(pid);
  const del = useDeleteProject();
  const navigate = useNavigate();
  const [f, setF] = useState({ name: "", description: "", repoUrl: "", defaultBranch: "" });
  const [confirm, setConfirm] = useState(false);
  useEffect(() => {
    if (p) setF({ name: p.name, description: p.description ?? "", repoUrl: p.repoUrl ?? "", defaultBranch: p.defaultBranch ?? "" });
  }, [p]);
  const set = (k: keyof typeof f) => (e: { target: { value: string } }) => setF({ ...f, [k]: e.target.value });
  return (
    <div className="space-y-8">
      <form
        className="max-w-xl space-y-4"
        onSubmit={(e) => {
          e.preventDefault();
          update.mutate(f);
        }}
      >
        <div className="space-y-2">
          <Label htmlFor="s-name">Name</Label>
          <Input id="s-name" value={f.name} onChange={set("name")} required />
        </div>
        <div className="space-y-2">
          <Label htmlFor="s-desc">Description</Label>
          <Textarea id="s-desc" value={f.description} onChange={set("description")} rows={3} />
        </div>
        <div className="space-y-2">
          <Label htmlFor="s-repo">Repository URL</Label>
          <Input id="s-repo" value={f.repoUrl} onChange={set("repoUrl")} type="url" />
        </div>
        <div className="space-y-2">
          <Label htmlFor="s-branch">Default branch</Label>
          <Input id="s-branch" value={f.defaultBranch} onChange={set("defaultBranch")} />
        </div>
        <Button type="submit" disabled={update.isPending}>
          Save
        </Button>
      </form>
      <Github pid={pid} owner />
      <div className="max-w-xl rounded-lg border border-destructive/40 p-4">
        <h3 className="text-sm font-medium text-destructive">Danger zone</h3>
        <p className="my-2 text-sm text-muted-foreground">Deleting the project removes its members, memory links and session access.</p>
        <Button variant="destructive" onClick={() => setConfirm(true)}>
          Delete project
        </Button>
      </div>
      <AlertDialog open={confirm} onOpenChange={setConfirm}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete {p?.name}?</AlertDialogTitle>
            <AlertDialogDescription>This cannot be undone.</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={() => del.mutate(pid, { onSuccess: () => void navigate({ to: "/" }) })}>Delete</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function Audit({ pid }: { pid: string }) {
  const a = useAudit(pid, 200);
  if (a.isLoading) return <ListSkeleton />;
  if (a.error) return <ErrorState error={a.error} retry={() => void a.refetch()} />;
  if (!a.data?.length) return <Empty title="No activity yet" />;
  return (
    <ul className="divide-y rounded-lg border">
      {a.data.map((e) => (
        <li key={e.id} className="flex items-center gap-3 p-3 text-sm">
          <UserAvatar login={e.actor} className="size-6" />
          <div className="min-w-0 flex-1">
            <span className="font-medium">{e.actor}</span>
            {e.onBehalfOf ? <span className="text-muted-foreground"> for {e.onBehalfOf}</span> : null} <span className="text-muted-foreground">{e.action}</span>
            {e.threadId ? (
              <Link to="/t/$threadId" params={{ threadId: e.threadId }} className="ml-2 text-xs underline">
                session
              </Link>
            ) : null}
          </div>
          <time className="shrink-0 text-xs text-muted-foreground" dateTime={e.at}>
            {ago(e.at)}
          </time>
        </li>
      ))}
    </ul>
  );
}

export function ProjectPage() {
  const { projectId } = useParams({ from: "/p/$projectId" });
  const { tab } = useSearch({ from: "/p/$projectId" });
  const navigate = useNavigate();
  const project = useProject(projectId);
  const overview = useOverview().data;
  const self = useMe().data?.user.login ?? "";
  const { newSession } = useDialogs();
  const stats = useMemoryStats({ project: projectId });
  const importAgents = useImportAgents(projectId);
  const { ready: ghReady } = useGithubConnector();
  if (project.isLoading) return <ListSkeleton />;
  if (project.error || !project.data) return <ErrorState error={project.error} retry={() => void project.refetch()} />;
  const p = project.data;
  const owner = isOwner(p.role);
  const threads = (overview?.projects.find((x) => x.id === projectId)?.threads ?? []).filter((t) => t.kind !== "supervisor").sort((a, b) => b.updatedAt.localeCompare(a.updatedAt));
  return (
    <ScrollArea className="h-full">
      <div className="mx-auto max-w-4xl space-y-6 p-4 md:p-8">
        <div className="flex flex-wrap items-start gap-3">
          <div className="min-w-0 flex-1">
            <h1 className="flex items-center gap-2 text-xl font-semibold">
              {p.name} <RoleBadge role={p.role} />
            </h1>
            {p.description ? <p className="mt-1 text-sm text-muted-foreground">{p.description}</p> : null}
            <div className="mt-2 flex flex-wrap items-center gap-3 text-xs text-muted-foreground">
              {p.repoUrl ? (
                <a href={p.repoUrl} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 hover:underline">
                  <ExternalLink className="size-3" /> {p.repoUrl}
                </a>
              ) : null}
              {p.defaultBranch ? (
                <span className="inline-flex items-center gap-1">
                  <GitBranch className="size-3" /> {p.defaultBranch}
                </span>
              ) : null}
            </div>
          </div>
          <div className="flex flex-wrap gap-2">
            {canWrite(p.role) ? (
              <Button
                variant="outline"
                disabled={importAgents.isPending || !ghReady}
                title={ghReady ? "Import AGENTS.md from the repository as a pinned memory" : "Connect GitHub (Profile) to import AGENTS.md"}
                onClick={() =>
                  importAgents.mutate(undefined, {
                    onSuccess: (e) => toast.success(`Imported ${e.title || "AGENTS.md"} as a pinned memory`),
                    onError: (e) => (e instanceof ApiError && e.status === 404 ? toast.info("No AGENTS.md found in the repository, or GitHub is not connected.") : toastError(e)),
                  })
                }
              >
                <FileText /> Import AGENTS.md
              </Button>
            ) : null}
            <Button variant="outline" onClick={() => downloadText(`/projects/${projectId}/memory/export`, "MEMORY.md").catch(toastError)}>
              <Download /> Export MEMORY.md
            </Button>
            {canWrite(p.role) ? (
              <Button onClick={() => newSession(projectId)}>
                <Plus /> New session
              </Button>
            ) : null}
          </div>
        </div>
        <Tabs value={tab ?? "overview"} onValueChange={(t) => void navigate({ to: "/p/$projectId", params: { projectId }, search: { tab: t as "overview" }, replace: true })}>
          <TabsList className="flex-wrap">
            <TabsTrigger value="overview">Sessions</TabsTrigger>
            <TabsTrigger value="members">Members</TabsTrigger>
            <TabsTrigger value="memory" className="gap-1.5">
              Memory
              {stats.data?.pending ? <span className="rounded-full bg-warning px-1.5 text-[10px] text-black" aria-label={`${stats.data.pending} to review`}>{stats.data.pending}</span> : null}
            </TabsTrigger>
            <TabsTrigger value="audit">Audit</TabsTrigger>
            {owner ? <TabsTrigger value="settings">Settings</TabsTrigger> : null}
          </TabsList>
          <TabsContent value="overview" className="mt-4 space-y-4">
            <Github pid={projectId} owner={owner} />
            {threads.length ? (
              <ul className="divide-y rounded-lg border">
                {threads.map((t) => (
                  <li key={t.id}>
                    <Link to="/t/$threadId" params={{ threadId: t.id }} className="flex items-center gap-3 p-3 hover:bg-accent">
                      <StatusDot status={t.status} />
                      <span className="min-w-0 flex-1 truncate text-sm">{t.title || "(untitled)"}</span>
                      {t.createdBy ? <UserAvatar login={t.createdBy} className="size-5" /> : null}
                      <span className="text-xs text-muted-foreground">{ago(t.updatedAt)}</span>
                    </Link>
                  </li>
                ))}
              </ul>
            ) : (
              <Empty title="No sessions yet">Start one to put an agent on this project.</Empty>
            )}
          </TabsContent>
          <TabsContent value="members" className="mt-4">
            <Members pid={projectId} owner={owner} self={self} />
          </TabsContent>
          <TabsContent value="memory" className="mt-4">
            <MemoryManager project={projectId} compact readOnly={!canWrite(p.role)} />
          </TabsContent>
          <TabsContent value="audit" className="mt-4">
            <Audit pid={projectId} />
          </TabsContent>
          {owner ? (
            <TabsContent value="settings" className="mt-4">
              <Settings pid={projectId} />
            </TabsContent>
          ) : null}
        </Tabs>
      </div>
    </ScrollArea>
  );
}
