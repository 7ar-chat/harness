// The explorer: project switcher, what needs the person, sessions, work, setup, and the personal space.
import { Download, Ellipsis, FileText, ChevronDown, ChevronRight, ChevronsUpDown, Inbox, LayoutDashboard, LogOut, Monitor, Moon, Plus, Sun, Trash2, UserCog, Users, Brain, WifiOff, Bot } from "lucide-react";
import { useState, type ReactNode } from "react";
import { useNavigate } from "@tanstack/react-router";
import { getDevUser, setDevUser } from "@/api/client";
import { useNeedsReview } from "@/api/factory";
import { useAdminUsers, useDeleteThread, useMe, useMemoryStats, useOverview, useSeen } from "@/api/queries";
import type { ThreadSummary } from "@/api/types";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuLabel, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { useFeed } from "@/lib/feed";
import { useTheme } from "@/lib/theme";
import { isUnread } from "@/lib/unread";
import { cn } from "@/lib/utils";
import { StatusDot, UserAvatar } from "../common";
import { useDialogs } from "../dialogs";
import { NavLink, useShell } from "./context";
import { useProjectActions } from "./projectActions";
import { projectHref } from "./tabs";

const byRecent = (a: ThreadSummary, b: ThreadSummary) => b.updatedAt.localeCompare(a.updatedAt);

function Section({ title, count, action, children, defaultOpen = true }: { title: string; count?: number; action?: ReactNode; children: ReactNode; defaultOpen?: boolean }) {
  const [open, setOpen] = useState(defaultOpen);
  return (
    <section aria-label={title} className="pb-1">
      <div className="group flex items-center pr-1">
        <button type="button" aria-expanded={open} onClick={() => setOpen(!open)} className="flex flex-1 items-center gap-1 rounded px-2.5 pb-1 pt-3 text-left text-[11px] font-medium uppercase tracking-[0.08em] text-muted-foreground hover:text-foreground focus-visible:outline-2">
          {open ? <ChevronDown className="size-3" aria-hidden /> : <ChevronRight className="size-3" aria-hidden />}
          {title}
          {count !== undefined ? <span> · {count}</span> : null}
        </button>
        {action}
      </div>
      {open ? <div className="flex flex-col gap-px">{children}</div> : null}
    </section>
  );
}

const rowCls = "group/row relative flex min-h-7 items-center gap-2 rounded-md px-2.5 py-1 text-[13px] text-sidebar-foreground outline-none hover:bg-sidebar-accent hover:text-sidebar-accent-foreground focus-visible:ring-2 focus-visible:ring-sidebar-ring aria-[current=page]:bg-sidebar-accent aria-[current=page]:font-medium aria-[current=page]:text-sidebar-accent-foreground";

function Row({ href, k, icon, children, meta, onNavigate, label }: { href: string; k: string; icon?: ReactNode; children: ReactNode; meta?: ReactNode; onNavigate?: () => void; label?: string }) {
  const { tabs } = useShell();
  return (
    <NavLink href={href} onNavigate={onNavigate} aria-current={tabs.active?.key === k ? "page" : undefined} aria-label={label} className={rowCls}>
      {icon}
      <span className="min-w-0 flex-1 truncate">{children}</span>
      {meta}
    </NavLink>
  );
}


function SessionRow({ t, owner, onNavigate }: { t: ThreadSummary; owner?: boolean; onNavigate?: () => void }) {
  const seen = useSeen();
  const me = useMe().data?.user.login;
  const del = useDeleteThread();
  const navigate = useNavigate();
  const { tabs } = useShell();
  const [confirm, setConfirm] = useState(false);
  const canDelete = !!me && (t.createdBy === me || owner || !t.projectId);
  const unread = isUnread(seen, t.id, t.updatedAt, tabs.active?.id);
  return (
    <div className="group/s relative">
      <NavLink href={`/t/${t.id}`} onNavigate={onNavigate} aria-current={tabs.active?.key === `t:${t.id}` ? "page" : undefined} className={cn(rowCls, canDelete && "pr-8")}>
        <StatusDot status={t.status} unread={unread} />
        <span className={cn("min-w-0 flex-1 truncate", unread && "font-semibold")}>{t.title || "(untitled)"}</span>
      </NavLink>
      {canDelete ? (
        <button type="button" onClick={() => setConfirm(true)} aria-label={`Delete ${t.title || "session"}`} title="Delete session" className="absolute right-1 top-1/2 hidden size-6 -translate-y-1/2 items-center justify-center rounded text-muted-foreground hover:bg-sidebar-accent hover:text-foreground focus-visible:flex group-hover/s:flex group-focus-within/s:flex">
          <Trash2 className="size-3.5" />
        </button>
      ) : null}
      <AlertDialog open={confirm} onOpenChange={setConfirm}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete “{t.title || "(untitled)"}”?</AlertDialogTitle>
            <AlertDialogDescription>The conversation and its workspace are removed for everyone.</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={() => del.mutate(t.id, { onSuccess: () => { tabs.close(`t:${t.id}`); if (tabs.active?.key === `t:${t.id}`) void navigate({ to: "/" }); } })}>Delete</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

export function UserMenu() {
  const me = useMe().data?.user;
  const { theme, setTheme } = useTheme();
  const [dev, setDev] = useState(getDevUser());
  const isAdmin = !!useMe().data?.admin;
  const { tabs } = useShell();
  const pendingUsers = useAdminUsers("pending", { enabled: isAdmin, refetchInterval: 30_000 }).data?.length ?? 0;
  const logout = async () => {
    try {
      await fetch("/auth/logout", { method: "POST", credentials: "same-origin" }).catch(() => undefined);
    } finally {
      location.assign("/");
    }
  };
  if (!me) return null;
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <button type="button" aria-label="User menu" className="flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left outline-none hover:bg-sidebar-accent focus-visible:ring-2 focus-visible:ring-sidebar-ring data-[state=open]:bg-sidebar-accent">
          <span className="relative shrink-0">
            <UserAvatar login={me.login} name={me.name} url={me.avatarUrl} className="size-6" />
            {pendingUsers ? <span className="absolute -right-0.5 -top-0.5 size-2.5 rounded-full bg-warning ring-2 ring-sidebar" aria-label={`${pendingUsers} users awaiting approval`} /> : null}
          </span>
          <span className="min-w-0 flex-1 truncate text-[13px] text-sidebar-foreground">{me.name || me.login}</span>
          <ChevronsUpDown className="size-3.5 text-muted-foreground" />
        </button>
      </DropdownMenuTrigger>
      <DropdownMenuContent side="top" align="start" className="w-64">
        <div className="flex items-center gap-2 px-2 py-1.5">
          <UserAvatar login={me.login} name={me.name} url={me.avatarUrl} className="size-8" />
          <div className="grid min-w-0 leading-tight">
            <span className="truncate text-sm font-medium">{me.name || me.login}</span>
            <span className="truncate text-xs text-muted-foreground">
              @{me.login}
              {isAdmin ? " · admin" : ""}
            </span>
          </div>
        </div>
        <DropdownMenuSeparator />
        <DropdownMenuItem onSelect={() => tabs.open("/profile")}>
          <UserCog /> Settings
        </DropdownMenuItem>
        <DropdownMenuItem onSelect={() => tabs.open("/memory")}>
          <Brain /> My memory
        </DropdownMenuItem>
        {isAdmin ? (
          <DropdownMenuItem onSelect={() => tabs.open("/admin")}>
            <Users /> Admin
            {pendingUsers ? <span className="ml-auto rounded-full bg-warning px-1.5 text-[10px] text-warning-foreground">{pendingUsers} pending</span> : null}
          </DropdownMenuItem>
        ) : null}
        <DropdownMenuSeparator />
        <DropdownMenuLabel>Theme</DropdownMenuLabel>
        {([["light", "Light", Sun], ["dark", "Dark", Moon], ["system", "System", Monitor]] as const).map(([v, label, Icon]) => (
          <DropdownMenuItem key={v} onSelect={() => setTheme(v)}>
            <Icon /> {label} {theme === v ? "✓" : ""}
          </DropdownMenuItem>
        ))}
        {import.meta.env.DEV ? (
          <>
            <DropdownMenuSeparator />
            <DropdownMenuLabel>Dev user (X-Harness-User)</DropdownMenuLabel>
            <form
              className="flex gap-1 px-2 pb-1"
              onSubmit={(e) => {
                e.preventDefault();
                setDevUser(dev.trim());
                location.reload();
              }}
            >
              <Input value={dev} onChange={(e) => setDev(e.target.value)} placeholder="alice" className="h-8" aria-label="Dev user" onKeyDown={(e) => e.stopPropagation()} />
              <Button size="sm" type="submit">
                Switch
              </Button>
            </form>
          </>
        ) : null}
        <DropdownMenuSeparator />
        <DropdownMenuItem onSelect={() => void logout()} className="text-destructive focus:text-destructive">
          <LogOut /> Log out
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

const SetupLink = ({ pid, section, k, label, onNavigate, meta }: { pid: string; section: string; k?: string; label: string; onNavigate?: () => void; meta?: ReactNode }) => (
  <Row href={projectHref(pid, section)} k={k ?? `p:${pid}:${section}`} onNavigate={onNavigate} meta={meta}>
    {label}
  </Row>
);

/** The project's actions (they used to be a header on every project page). */
function ProjectMenu() {
  const a = useProjectActions(useShell().projectId);
  const { openSwitcher } = useShell();
  const { project } = a ?? {};
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <button type="button" aria-label="Project actions" className="flex size-7 shrink-0 items-center justify-center rounded-md text-muted-foreground outline-none hover:bg-sidebar-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-sidebar-ring data-[state=open]:bg-sidebar-accent">
          <Ellipsis className="size-4" />
        </button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-56">
        {a && project ? (
          <>
            <DropdownMenuLabel className="truncate">
              {project.name} · {project.role}
            </DropdownMenuLabel>
            {a.canWrite ? (
              <DropdownMenuItem onSelect={a.newSession}>
                <Plus /> New session
              </DropdownMenuItem>
            ) : null}
            {a.canWrite ? (
              <DropdownMenuItem disabled={!a.canImport} title={a.importHint} onSelect={a.importAgents}>
                <FileText /> Import AGENTS.md
              </DropdownMenuItem>
            ) : null}
            <DropdownMenuItem onSelect={a.exportMemory}>
              <Download /> Export MEMORY.md
            </DropdownMenuItem>
            <DropdownMenuSeparator />
          </>
        ) : null}
        <DropdownMenuItem onSelect={openSwitcher}>
          <ChevronsUpDown /> Switch project <span className="ml-auto text-xs text-muted-foreground">⌘P</span>
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

export function ExplorerBody({ onNavigate }: { onNavigate?: () => void }) {
  const overview = useOverview().data;
  const review = useNeedsReview().data;
  const { newSession, newProject } = useDialogs();
  const { projectId, openSwitcher, narrow } = useShell();
  const { conn } = useFeed();
  const project = overview?.projects.find((p) => p.id === projectId);
  const threads = (project?.threads ?? []).filter((t) => t.kind !== "supervisor").sort(byRecent);
  const personal = (overview?.personal ?? []).filter((t) => t.kind !== "supervisor").sort(byRecent);
  const total = review?.count ?? 0;
  const memPending = useMemoryStats({ project: projectId ?? "" }, !!projectId).data?.pending ?? 0;
  const mine = (pid: string) => !projectId || pid === projectId;
  const needs = [
    ...(review?.gates ?? []).filter((g) => mine(g.projectId)).map((g) => ({ key: `g:${g.runId}:${g.nodeId}`, tone: "text-warning", label: `${g.workflowName}: ${g.title}`, href: projectHref(g.projectId, "workflows", { run: g.runId }), k: `p:${g.projectId}:run:${g.runId}`, kind: "gate" })),
    ...(review?.tasks ?? []).filter((it) => mine(it.projectId)).map((it) => ({ key: `t:${it.task.id}`, tone: it.kind === "blocked" ? "text-destructive" : "text-warning", label: it.task.title, href: projectHref(it.projectId, "tasks", { task: it.task.id }), k: `p:${it.projectId}:task:${it.task.id}`, kind: it.kind === "blocked" ? "blocked" : "review" })),
    ...(review?.threads ?? []).filter((it) => mine(it.projectId ?? "")).map((it) => ({ key: `s:${it.thread.id}`, tone: "text-warning", label: it.thread.title || "(untitled)", href: `/t/${it.thread.id}`, k: `t:${it.thread.id}`, kind: "session" })),
  ];
  const visible = needs.slice(0, 6);
  const personalCount = personal.length;
  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className={cn("flex shrink-0 items-center gap-2 px-2 pb-1 pt-2", narrow && "pr-12")}>
        <button type="button" onClick={openSwitcher} aria-label="Switch project" title="Switch project (Ctrl/Cmd+P)" className="flex min-w-0 flex-1 items-center gap-2 rounded-md px-2 py-1.5 text-left outline-none hover:bg-sidebar-accent focus-visible:ring-2 focus-visible:ring-sidebar-ring">
          <span aria-hidden className="flex size-[22px] shrink-0 items-center justify-center rounded-md bg-primary text-xs font-bold text-primary-foreground">
            7
          </span>
          <span className="min-w-0 flex-1 truncate font-semibold text-foreground">{project?.name ?? (overview ? "Personal" : "7ar")}</span>
          {narrow ? null : <span className="shrink-0 text-[11px] text-muted-foreground">switch ⌘P</span>}
        </button>
        <ProjectMenu />
        {conn === "offline" ? <WifiOff className="size-4 shrink-0 text-destructive" aria-label="Offline, reconnecting" /> : null}
      </div>
      <nav aria-label="Explorer" className="min-h-0 flex-1 overflow-y-auto overflow-x-hidden px-2 pb-2">
        <div className="flex flex-col gap-px pt-1">
          <Row href="/" k="home" icon={<LayoutDashboard className="size-4 text-muted-foreground" />} onNavigate={onNavigate}>
            Dashboard
          </Row>
          <Row
            href="/inbox"
            k="page:/inbox"
            icon={<Inbox className="size-4 text-muted-foreground" />}
            onNavigate={onNavigate}
            meta={total ? <span className="rounded-full bg-warning px-1.5 text-[10px] font-medium text-warning-foreground" aria-label={`${total} waiting for you`}>{total}</span> : null}
          >
            Inbox
          </Row>
        </div>

        {project ? (
          <>
            <Section title="Needs you" count={needs.length}>
              {visible.length ? (
                visible.map((n) => (
                  <Row key={n.key} href={n.href} k={n.k} onNavigate={onNavigate} icon={<span aria-hidden className={cn("text-[10px]", n.tone)}>●</span>}>
                    {n.label}
                  </Row>
                ))
              ) : (
                <p className="px-2.5 py-1 text-xs text-muted-foreground">Nothing waits for you.</p>
              )}
              {needs.length > visible.length ? (
                <Row href="/inbox" k="page:/inbox" onNavigate={onNavigate}>
                  <span className="text-muted-foreground">{needs.length - visible.length} more in the inbox</span>
                </Row>
              ) : null}
            </Section>

            <Section
              title="Sessions"
              action={
                project.role !== "viewer" ? (
                  <button type="button" onClick={() => newSession(project.id)} aria-label={`New session in ${project.name}`} title="New session" className="flex size-6 items-center justify-center rounded text-muted-foreground hover:bg-sidebar-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-sidebar-ring">
                    <Plus className="size-3.5" />
                  </button>
                ) : null
              }
            >
              {threads.length ? (
                <>
                  {threads.slice(0, 12).map((t) => (
                    <SessionRow key={t.id} t={t} owner={project.role === "owner"} onNavigate={onNavigate} />
                  ))}
                  {threads.length > 12 ? (
                    <Row href={projectHref(project.id, "overview")} k={`p:${project.id}:overview`} onNavigate={onNavigate}>
                      <span className="text-muted-foreground">All {threads.length} sessions</span>
                    </Row>
                  ) : null}
                </>
              ) : (
                <p className="px-2.5 py-1 text-xs text-muted-foreground">No sessions yet</p>
              )}
            </Section>

            <Section title="Work">
              <SetupLink pid={project.id} section="board" label="Board" onNavigate={onNavigate} />
              <SetupLink pid={project.id} section="requirements" label="Requirements" onNavigate={onNavigate} />
              <SetupLink pid={project.id} section="tasks" label="Tasks & triggers" onNavigate={onNavigate} />
              <SetupLink pid={project.id} section="workflows" label="Workflows & runs" onNavigate={onNavigate} />
              <SetupLink pid={project.id} section="insights" label="Insights" onNavigate={onNavigate} />
            </Section>

            <Section title="Setup">
              <SetupLink pid={project.id} section="roles" label="Roles & executors" onNavigate={onNavigate} />
              <SetupLink pid={project.id} section="schedules" label="Schedules" onNavigate={onNavigate} />
              <SetupLink pid={project.id} section="recipes" label="Recipes" onNavigate={onNavigate} />
              <SetupLink pid={project.id} section="memory" label="Memory & docs" onNavigate={onNavigate} meta={memPending ? <span className="rounded-full bg-warning px-1.5 text-[10px] font-medium text-warning-foreground" aria-label={`${memPending} memories to review`}>{memPending}</span> : null} />
              <SetupLink pid={project.id} section="members" label="Members & access" onNavigate={onNavigate} />
              <SetupLink pid={project.id} section="capabilities" label="Capabilities" onNavigate={onNavigate} />
              <SetupLink pid={project.id} section="audit" label="Audit log" onNavigate={onNavigate} />
            </Section>
          </>
        ) : overview ? (
          <p className="px-2.5 py-3 text-xs text-muted-foreground">
            No project yet.{" "}
            <button type="button" className="text-primary underline underline-offset-2" onClick={newProject}>
              Create one
            </button>
          </p>
        ) : null}

        <Section
          title="Personal"
          defaultOpen={!project || personalCount > 0}
          action={
            <button type="button" onClick={() => newSession()} aria-label="New personal session" title="New session" className="flex size-6 items-center justify-center rounded text-muted-foreground hover:bg-sidebar-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-sidebar-ring">
              <Plus className="size-3.5" />
            </button>
          }
        >
          <Row href="/supervisor" k="page:/supervisor" icon={<Bot className="size-4 text-muted-foreground" />} onNavigate={onNavigate}>
            My assistant
          </Row>
          {personal.slice(0, 8).map((t) => (
            <SessionRow key={t.id} t={t} onNavigate={onNavigate} />
          ))}
          <Row href="/jobs" k="page:/jobs" onNavigate={onNavigate}>
            Jobs
          </Row>
          <Row href="/references" k="page:/references" onNavigate={onNavigate}>
            References
          </Row>
          <Row href="/connectors" k="page:/connectors" onNavigate={onNavigate}>
            Connectors
          </Row>
          <Row href="/memory" k="page:/memory" onNavigate={onNavigate}>
            Library
          </Row>
          <Row href="/schedules" k="page:/schedules" onNavigate={onNavigate}>
            My schedules
          </Row>
          <Row href="/capabilities" k="page:/capabilities" onNavigate={onNavigate}>
            My capabilities
          </Row>
        </Section>
      </nav>
      <div className="shrink-0 border-t border-sidebar-border px-2 py-1">
        {project ? (
          <>
            <Row href={projectHref(project.id, "usage")} k={`p:${project.id}:usage`} onNavigate={onNavigate}>
              Usage & caps
            </Row>
            {project.role === "owner" ? (
              <Row href={projectHref(project.id, "settings")} k={`p:${project.id}:settings`} onNavigate={onNavigate}>
                Settings
              </Row>
            ) : null}
          </>
        ) : (
          <>
            <Row href="/insights" k="page:/insights" onNavigate={onNavigate}>
              My insights
            </Row>
            <Row href="/usage" k="page:/usage" onNavigate={onNavigate}>
              Usage & caps
            </Row>
          </>
        )}
        <UserMenu />
      </div>
    </div>
  );
}
