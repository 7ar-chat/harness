// The explorer: project switcher, what needs the person, sessions, work, setup, and the personal space.
import { CircleHelp, Compass, Download, Ellipsis, FileText, ChevronDown, ChevronRight, ChevronsUpDown, Inbox, LayoutDashboard, LogOut, Monitor, Moon, Plus, Sun, Trash2, UserCog, Users, Brain, WifiOff, Bot } from "lucide-react";
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
import { onboardingAuto, requestOnboarding } from "@/lib/onboarding";
import { GettingStarted } from "../onboarding/GettingStarted";
import { useTheme } from "@/lib/theme";
import { rememberUi } from "@/lib/serverPrefs";
import { isUnread } from "@/lib/unread";
import { cn } from "@/lib/utils";
import { StatusDot, UserAvatar } from "../common";
import { useDialogs } from "../dialogs";
import { NavLink, useShell } from "./context";
import { useProjectActions } from "./projectActions";
import { ME_AREA, PROJECT_AREAS, areaHref, locateTab } from "./areas";
import { projectHref, type EditorTab } from "./tabs";

const byRecent = (a: ThreadSummary, b: ThreadSummary) => b.updatedAt.localeCompare(a.updatedAt);

const SECTION_KEY = "7ar.ui.section.";
const readSection = (title: string): boolean | undefined => {
  try {
    const v = localStorage.getItem(SECTION_KEY + title);
    return v === "1" ? true : v === "0" ? false : undefined;
  } catch {
    return undefined;
  }
};

/** A foldable group. A person's choice is remembered; `forceOpen` (the shown page is inside it) opens it without remembering. */
function Section({ title, label, count, action, badge, children, defaultOpen = true, forceOpen = false }: { title: string; label?: string; count?: number; action?: ReactNode; badge?: ReactNode; children: ReactNode; defaultOpen?: boolean; forceOpen?: boolean }) {
  const [open, setOpen] = useState(() => readSection(title) ?? defaultOpen);
  const shown = open || forceOpen;
  const toggle = () => {
    const next = !shown;
    setOpen(next);
    try {
      localStorage.setItem(SECTION_KEY + title, next ? "1" : "0");
    } catch {
      /* the choice just is not remembered */
    }
    rememberUi({ section: [title, next] });
  };
  return (
    <section aria-label={label ?? title} className="pb-1">
      <div className="group flex items-center pr-1">
        <button type="button" aria-expanded={shown} onClick={toggle} className="flex flex-1 items-center gap-1 rounded px-2.5 pb-1 pt-3 text-left text-[11px] font-medium uppercase tracking-[0.08em] text-muted-foreground hover:text-foreground focus-visible:outline-2">
          {shown ? <ChevronDown className="size-3" aria-hidden /> : <ChevronRight className="size-3" aria-hidden />}
          {title}
          {count !== undefined ? <span> · {count}</span> : null}
          {!shown && badge ? <span className="ml-1.5 normal-case tracking-normal">{badge}</span> : null}
        </button>
        {action}
      </div>
      {shown ? <div className="flex flex-col gap-px">{children}</div> : null}
    </section>
  );
}

const rowCls = "group/row relative flex min-h-7 items-center gap-2 rounded-md px-2.5 py-1 text-[13px] text-sidebar-foreground outline-none hover:bg-sidebar-accent hover:text-sidebar-accent-foreground focus-visible:ring-2 focus-visible:ring-sidebar-ring aria-[current=page]:bg-sidebar-accent aria-[current=page]:font-medium aria-[current=page]:text-sidebar-accent-foreground";

function Row({ href, k, icon, children, meta, onNavigate, label, title, current }: { href: string; k: string; icon?: ReactNode; children: ReactNode; meta?: ReactNode; onNavigate?: () => void; label?: string; title?: string; current?: boolean }) {
  const { tabs } = useShell();
  return (
    <NavLink href={href} onNavigate={onNavigate} aria-current={(current ?? tabs.active?.key === k) ? "page" : undefined} aria-label={label} title={title} className={rowCls}>
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
            <AlertDialogDescription>The session and its workspace are removed for everyone.</AlertDialogDescription>
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
        <button type="button" className="flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left outline-none hover:bg-sidebar-accent focus-visible:ring-2 focus-visible:ring-sidebar-ring data-[state=open]:bg-sidebar-accent">
          <span className="relative shrink-0">
            <span aria-hidden>
              <UserAvatar login={me.login} name={me.name} url={me.avatarUrl} className="size-6" />
            </span>
            {pendingUsers ? <span className="absolute -right-0.5 -top-0.5 size-2.5 rounded-full bg-warning ring-2 ring-sidebar" aria-label={`${pendingUsers} users awaiting approval`} /> : null}
          </span>
          <span className="min-w-0 flex-1 truncate text-[13px] text-sidebar-foreground">
            <span className="sr-only">User menu: </span>
            {me.name || me.login}
          </span>
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
          <UserCog /> My settings
        </DropdownMenuItem>
        <DropdownMenuItem onSelect={() => tabs.open("/memory")}>
          <Brain /> My memory
        </DropdownMenuItem>
        <DropdownMenuSeparator />
        <DropdownMenuItem onSelect={() => requestOnboarding("welcome")}>
          <Compass /> Getting started
        </DropdownMenuItem>
        <DropdownMenuItem onSelect={() => requestOnboarding("tour")}>
          <Compass /> Take the tour
        </DropdownMenuItem>
        <DropdownMenuItem onSelect={() => tabs.open("/help")}>
          <CircleHelp /> Help
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

/** The area an editor tab belongs to ("work", "settings", "me"), whatever view or detail it shows. */
function areaOfTab(t: EditorTab | undefined, projectId?: string): string | undefined {
  if (!t) return undefined;
  if (t.key === "page:me") return "me";
  if (t.projectId && t.projectId === projectId) return locateTab(t.section ?? "board")?.area.id;
  return undefined;
}

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
  const { projectId, openSwitcher, narrow, tabs } = useShell();
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
        <button type="button" onClick={openSwitcher} title="Switch project (Ctrl/Cmd+P)" className="flex min-w-0 flex-1 items-center gap-2 rounded-md px-2 py-1.5 text-left outline-none hover:bg-sidebar-accent focus-visible:ring-2 focus-visible:ring-sidebar-ring">
          <span aria-hidden className="flex size-[22px] shrink-0 items-center justify-center rounded-md bg-primary text-xs font-bold text-primary-foreground">
            7
          </span>
          <span className="min-w-0 flex-1 truncate font-semibold text-foreground">
            <span className="sr-only">Switch project: </span>
            {project?.name ?? (overview ? "Personal" : "7ar")}
          </span>
          {narrow ? null : (
            <span aria-hidden className="shrink-0 text-[11px] text-muted-foreground">
              switch ⌘P
            </span>
          )}
        </button>
        <ProjectMenu />
        {conn === "offline" ? <WifiOff className="size-4 shrink-0 text-destructive" aria-label="Offline, reconnecting" /> : null}
      </div>
      <nav aria-label="Explorer" className="min-h-0 flex-1 overflow-y-auto overflow-x-hidden px-2 pb-2">
        {onboardingAuto() ? <GettingStarted variant="explorer" projectId={projectId} onNavigate={onNavigate} /> : null}
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
            Needs you
          </Row>
        </div>

        {project ? (
          <>
            {needs.length ? (
            <Section title="Needs you" label="Needs you in the explorer" count={needs.length}>
              {visible.length ? (
                visible.map((n) => (
                  <Row key={n.key} href={n.href} k={n.k} onNavigate={onNavigate} icon={<span aria-hidden className={cn("text-[10px]", n.tone)}>●</span>}>
                    {n.label}
                  </Row>
                ))
              ) : null}
              {needs.length > visible.length ? (
                <Row href="/inbox" k="page:/inbox" onNavigate={onNavigate}>
                  <span className="text-muted-foreground">{needs.length - visible.length} more in Needs you</span>
                </Row>
              ) : null}
            </Section>
            ) : null}

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

            <Section title="Project">
              {PROJECT_AREAS.map((area) => (
                <Row
                  key={area.id}
                  href={areaHref(area, project.id, project.role === "owner")}
                  k={`area:${area.id}`}
                  current={areaOfTab(tabs.active, project.id) === area.id}
                  onNavigate={onNavigate}
                  title={area.hint}
                  meta={area.id === "settings" && memPending ? <span className="rounded-full bg-warning px-1.5 text-[10px] font-medium text-warning-foreground" aria-label={`${memPending} memories to review`}>{memPending}</span> : null}
                >
                  {area.label}
                </Row>
              ))}
            </Section>
          </>
        ) : overview && overview.projects.length ? (
          <p className="px-2.5 py-3 text-xs text-muted-foreground">Personal. Pick a project with the switcher above to see its work.</p>
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
          <Row href={areaHref(ME_AREA)} k="area:me" current={areaOfTab(tabs.active, projectId) === "me"} onNavigate={onNavigate} title={ME_AREA.hint}>
            {ME_AREA.label}
          </Row>
        </Section>
      </nav>
      <div className="shrink-0 border-t border-sidebar-border px-2 py-1">
        <Row href="/profile" k="page:/profile" onNavigate={onNavigate} title="Your account and preferences">
          Settings
        </Row>
        <UserMenu />
      </div>
    </div>
  );
}
