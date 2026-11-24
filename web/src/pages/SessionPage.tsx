import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate, useParams } from "@tanstack/react-router";
import { ModelPicker } from "@/components/app/ModelPicker";
import { Bot, ChevronDown, ChevronUp, GitBranch, Pencil, SlidersHorizontal, Square, Trash2 } from "lucide-react";
import { useCallback, useMemo, useState } from "react";
import { ApiError, canWrite } from "@/api/client";
import { supervisorQuery, threadQuery, useDeleteThread, useMe, useMembers, useModels, useOverview, useRenameThread, useSupervisorSettings, useSaveSupervisorSettings } from "@/api/queries";
import type { Author, PlanItem } from "@/api/types";
import { PublishedList } from "@/chat/artifacts";
import { ChatView } from "@/chat/ChatView";
import { EnvironmentControl, useEnvironments } from "@/chat/environment";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { ErrorState, ListSkeleton, StatusBadge, UserAvatar } from "@/components/app/common";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { ResizableHandle, ResizablePanel, ResizablePanelGroup } from "@/components/ui/resizable";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { useIsMobile } from "@/hooks/use-mobile";
import { useRunSettings } from "@/lib/hooks";
import { MemoryManager } from "./Memory";
import { DiffPanel, sessionEdits } from "./DiffPanel";
import { ActivityPanel, FilesPanel, PlanPanel, PreviewPanel } from "./panels";
import { SessionCard } from "./Dashboard";

const SUGGESTIONS = ["What's running?", "Anything stuck?", "Approve safe pending requests"];

function TitleEditor({ id, title, editable }: { id: string; title: string; editable: boolean }) {
  const [editing, setEditing] = useState(false);
  const [v, setV] = useState(title);
  const rename = useRenameThread(id);
  if (!editing)
    return (
      <div className="flex min-w-0 items-center gap-1">
        <h1 className="truncate text-base font-semibold">{title || "(untitled)"}</h1>
        {editable ? (
          <Button variant="ghost" size="icon" className="size-7" aria-label="Rename session" onClick={() => (setV(title), setEditing(true))}>
            <Pencil />
          </Button>
        ) : null}
      </div>
    );
  const save = () => {
    setEditing(false);
    if (v.trim() && v.trim() !== title) rename.mutate(v.trim());
  };
  return <Input autoFocus value={v} onChange={(e) => setV(e.target.value)} onBlur={save} onKeyDown={(e) => (e.key === "Enter" ? save() : e.key === "Escape" ? setEditing(false) : null)} className="h-8 max-w-sm" aria-label="Session title" />;
}

export function SupervisorSettingsBar() {
  const s = useSupervisorSettings().data;
  const save = useSaveSupervisorSettings();
  if (!s) return null;
  return (
    <div className="flex flex-wrap items-center gap-3 text-xs">
      <label className="flex items-center gap-2">
        <Switch checked={s.watch} onCheckedChange={(watch) => save.mutate({ ...s, watch })} aria-label="Watch my sessions for me" /> Watch my sessions for me
      </label>
      <label className="flex items-center gap-2">
        Stuck after
        <Input type="number" min={1} defaultValue={s.stuckMinutes} className="h-7 w-16" aria-label="Stuck minutes" onBlur={(e) => Number(e.target.value) > 0 && Number(e.target.value) !== s.stuckMinutes && save.mutate({ ...s, stuckMinutes: Number(e.target.value) })} />
        min
      </label>
    </div>
  );
}

function SessionsList() {
  const o = useOverview().data;
  const rows = [...(o?.personal ?? []).map((t) => ({ ...t, projectName: "Personal" })), ...(o?.projects ?? []).flatMap((p) => p.threads.map((t) => ({ ...t, projectName: p.name })))].filter((t) => t.kind !== "supervisor" && t.status !== "idle");
  return rows.length ? <div className="space-y-2 p-3">{rows.map((t) => <SessionCard key={t.id} t={t} />)}</div> : <p className="p-4 text-sm text-muted-foreground">No sessions are running.</p>;
}

export function SessionView({ threadId, supervisor }: { threadId: string; supervisor?: boolean }) {
  const q = useQuery(threadQuery(threadId));
  const models = useModels().data;
  const me = useMe().data?.user;
  const t = q.data;
  const members = useMembers(t?.projectId ?? "");
  const [settings, setSettings] = useRunSettings(models?.default);
  const navigate = useNavigate();
  const del = useDeleteThread();
  const qc = useQueryClient();
  const isMobile = useIsMobile();
  const [details, setDetails] = useState(false);
  const envs = useEnvironments().data;
  const [pickedTab, setTab] = useState<string>();
  const [confirm, setConfirm] = useState(false);
  const [status, setStatus] = useState<{ running: boolean; cancel: () => void; plan?: PlanItem[] }>({ running: false, cancel: () => {} });
  const project = useMe().data?.projects.find((p) => p.id === t?.projectId);
  const role = t?.role ?? project?.role;
  const readOnly = !canWrite(role);
  const users = useCallback(
    (login: string): Author | undefined => {
      const m = members.data?.find((x) => x.login === login);
      if (m) return { login, name: m.name, avatarUrl: m.avatarUrl };
      return me && me.login === login ? { login, name: me.name, avatarUrl: me.avatarUrl } : { login };
    },
    [members.data, me],
  );
  const self = useMemo(() => (me ? { login: me.login, name: me.name, avatarUrl: me.avatarUrl } : undefined), [me]);
  const participants = useMemo(() => {
    const logins = new Set<string>();
    if (t?.createdBy) logins.add(t.createdBy);
    for (const m of t?.messages ?? []) if (m.author?.login) logins.add(m.author.login);
    return [...logins].map((l) => users(l) ?? { login: l });
  }, [t, users]);

  if (q.isLoading) return <ListSkeleton rows={6} />;
  if (q.error || !t) return <ErrorState error={q.error} retry={() => void q.refetch()} />;
  const canDelete = t.createdBy === me?.login || role === "owner" || !t.projectId;
  const plan = status.plan ?? t.plan ?? [];
  const edits = sessionEdits(t.messages ?? []);
  const tab = pickedTab ?? (edits.length ? "diff" : "files");
  const infoModels = models?.info;

  const side = supervisor ? (
    <div className="h-full overflow-y-auto">
      <div className="border-b p-3">
        <SupervisorSettingsBar />
      </div>
      <SessionsList />
    </div>
  ) : (
    <Tabs value={tab} onValueChange={setTab} className="flex h-full min-h-0 flex-col gap-0 bg-panel">
      <TabsList variant="line" className="h-auto w-full shrink-0 justify-start gap-0 overflow-x-auto rounded-none border-b p-0">
        {[
          ["diff", "Diff" + (edits.length ? ` (${edits.length})` : ""), true],
          ["files", "Files", true],
          ["plan", "Plan" + (plan.length ? ` (${plan.length})` : ""), true],
          ["preview", "Preview", !!t.projectId],
          ["memory", "Memory", true],
          ["activity", "Activity", true],
        ].map(([v, label, show]) =>
          show ? (
            <TabsTrigger key={v as string} value={v as string} className="h-auto flex-none rounded-none px-3 py-2 text-xs group-data-[orientation=horizontal]/tabs:after:bottom-0 after:bg-primary">
              {label}
            </TabsTrigger>
          ) : null,
        )}
      </TabsList>
      <TabsContent value="diff" className="min-h-0 flex-1 overflow-y-auto">
        <DiffPanel messages={t.messages ?? []} />
      </TabsContent>
      <TabsContent value="files" className="min-h-0 flex-1 overflow-hidden">
        <div className="flex h-full min-h-0 flex-col">
          <PublishedList threadId={threadId} refreshKey={t.updatedAt + String(status.running)} />
          <div className="min-h-0 flex-1">
            <FilesPanel threadId={threadId} refreshKey={t.updatedAt + String(status.running)} />
          </div>
        </div>
      </TabsContent>
      {t.projectId ? (
        <TabsContent value="preview" className="min-h-0 flex-1 overflow-hidden">
          <PreviewPanel threadId={threadId} active={tab === "preview"} />
        </TabsContent>
      ) : null}
      <TabsContent value="plan" className="min-h-0 flex-1 overflow-y-auto">
        <PlanPanel items={plan} />
      </TabsContent>
      <TabsContent value="memory" className="min-h-0 flex-1 overflow-y-auto">
        <MemoryManager project={t.projectId} compact readOnly={readOnly} />
      </TabsContent>
      <TabsContent value="activity" className="min-h-0 flex-1 overflow-y-auto">
        <ActivityPanel projectId={t.projectId} threadId={threadId} />
      </TabsContent>
    </Tabs>
  );

  const chat = (
    <ChatView
      key={threadId}
      threadId={threadId}
      settings={settings}
      readOnly={readOnly}
      users={users}
      self={self}
      models={infoModels}
      suggestions={supervisor ? SUGGESTIONS : undefined}
      onStatus={setStatus}
      projectId={t.projectId}
      emptyHint={supervisor ? "Ask your assistant about your running sessions. It can watch them, answer for you, and nudge stuck ones." : "Send a message to start. Everyone with access to this project can see this session."}
    />
  );

  return (
    <div className="flex h-full min-h-0 flex-col">
      <header className="shrink-0 space-y-1.5 border-b px-3 py-2">
        <div className="flex flex-wrap items-center gap-2">
          {supervisor ? <Bot className="size-5" /> : null}
          {supervisor ? <h1 className="text-base font-semibold">My assistant</h1> : <TitleEditor id={threadId} title={t.title} editable={!readOnly} />}
          <div className="ml-auto flex items-center gap-2">
            <div className="flex -space-x-1.5">
              {participants.slice(0, 5).map((a) => (
                <UserAvatar key={a.login} login={a.login} name={a.name} url={a.avatarUrl} className="size-6 ring-2 ring-background" />
              ))}
            </div>
            {status.running ? (
              <Button size="sm" variant="destructive" onClick={status.cancel} disabled={readOnly}>
                <Square /> Cancel
              </Button>
            ) : null}
            <Popover>
              <PopoverTrigger asChild>
                <Button size="icon" variant="ghost" className="size-8" aria-label="Run settings">
                  <SlidersHorizontal />
                </Button>
              </PopoverTrigger>
              <PopoverContent align="end" className="w-64 space-y-3 text-xs">
                <div className="space-y-1.5">
                  <div className="font-medium">Mode</div>
                  <Tabs value={settings.mode} onValueChange={(m) => setSettings({ mode: m === "plan" ? "plan" : "build" })}>
                    <TabsList className="h-7">
                      <TabsTrigger value="build" className="px-2 text-xs">Build</TabsTrigger>
                      <TabsTrigger value="plan" className="px-2 text-xs">Plan</TabsTrigger>
                    </TabsList>
                  </Tabs>
                </div>
                <div className="flex items-center gap-2">
                  <Switch id="interactive" checked={settings.interactive} onCheckedChange={(interactive) => setSettings({ interactive })} />
                  <Label htmlFor="interactive" className="text-xs font-normal">Ask before risky tools</Label>
                </div>
              </PopoverContent>
            </Popover>
            {!supervisor && canDelete ? (
              <Button size="icon" variant="ghost" className="size-8" aria-label="Delete session" onClick={() => setConfirm(true)}>
                <Trash2 />
              </Button>
            ) : null}
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-1.5 text-xs">
          {t.agentRole ? (
            <span className="rounded-md bg-secondary px-2 py-0.5 font-mono" title="The role the last reply took on" data-testid="agent-role">
              role {t.agentRole}
            </span>
          ) : null}
          <ModelPicker
            models={models?.models ?? []}
            value={settings.model ?? ""}
            onChange={(model) => setSettings({ model })}
            info={models?.info}
            disabled={!models?.models.length}
            placeholder={models && !models.models.length ? "No model configured" : "Model"}
          />
          {supervisor ? null : envs?.selectable ? <EnvironmentControl threadId={threadId} projectId={t.projectId} readOnly={readOnly} /> : <span className="rounded-md bg-secondary px-2 py-0.5 font-mono">sandbox {envs?.default ?? "default"}</span>}
          {settings.mode === "plan" ? <span className="rounded-md bg-secondary px-2 py-0.5 font-mono">plan mode</span> : null}
          <StatusBadge status={t.status} />
          {t.branch ? (
            <span className="inline-flex items-center gap-1 rounded-md bg-secondary px-2 py-0.5 font-mono">
              <GitBranch className="size-3" /> {t.branch}
            </span>
          ) : null}
          {models && !models.models.length ? <span className="text-destructive">The server has no model configured, so runs cannot start.</span> : null}
        </div>
      </header>
      <div className="min-h-0 flex-1">
        {isMobile ? (
          <div className="flex h-full min-h-0 flex-col">
            <div className="min-h-0 flex-1">{chat}</div>
            <section aria-label="Session details" className="shrink-0 border-t bg-panel">
              <button type="button" aria-expanded={details} onClick={() => setDetails(!details)} className="flex w-full items-center gap-2 px-3 py-2 text-left text-xs text-muted-foreground">
                {details ? <ChevronDown className="size-3.5" /> : <ChevronUp className="size-3.5" />}
                Diff, files, plan, memory and activity
              </button>
              {details ? <div className="h-[42vh] border-t">{side}</div> : null}
            </section>
          </div>
        ) : (
          <ResizablePanelGroup orientation="horizontal">
            <ResizablePanel defaultSize="68%" minSize="35%">
              {chat}
            </ResizablePanel>
            <ResizableHandle withHandle />
            <ResizablePanel defaultSize="32%" minSize="20%">
              {side}
            </ResizablePanel>
          </ResizablePanelGroup>
        )}
      </div>
      <AlertDialog open={confirm} onOpenChange={setConfirm}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete this session?</AlertDialogTitle>
            <AlertDialogDescription>The conversation and its workspace are removed for everyone.</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() =>
                del.mutate(threadId, {
                  onSuccess: () => {
                    qc.removeQueries({ queryKey: threadQuery(threadId).queryKey });
                    void navigate({ to: "/" });
                  },
                })
              }
            >
              Delete
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

export function SessionPage() {
  const { threadId } = useParams({ from: "/t/$threadId" });
  return <SessionView threadId={threadId} />;
}

/** A session tab: it is told its thread (the shell keeps several tabs mounted, the address only names the active one). */
export const SessionTab = ({ threadId }: { threadId: string }) => <SessionView threadId={threadId} />;

export function SupervisorPage() {
  const q = useQuery(supervisorQuery);
  if (q.isLoading) return <ListSkeleton rows={5} />;
  if (q.error) return <ErrorState error={q.error} retry={() => void q.refetch()} />;
  if (!q.data) return <ErrorState error={new ApiError(404, "not_found", "Your assistant is not available on this server.")} />;
  return <SessionView threadId={q.data.id} supervisor />;
}
