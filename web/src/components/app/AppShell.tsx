import { Logo } from "@/components/app/Logo";
import { identifyUser } from "@/lib/telemetry";
import { Link, Outlet, useLocation, useNavigate, useParams } from "@tanstack/react-router";
import { Bot, Brain, ChevronRight, ChevronsUpDown, FileText, LayoutDashboard, Link2, Plug, LogOut, Monitor, Moon, Plus, Search, SlidersHorizontal, Sun, Trash2, UserCog, Users, WifiOff } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { getDevUser, setDevUser } from "@/api/client";
import { supervisorQuery, useAdminUsers, useDeleteThread, useMe, useMemoryStats, useOverview, useSeen } from "@/api/queries";
import type { OverviewProject, ThreadSummary } from "@/api/types";
import { useQuery } from "@tanstack/react-query";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { CommandDialog, CommandEmpty, CommandGroup, CommandInput, CommandItem, CommandList } from "@/components/ui/command";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuLabel, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Sidebar, SidebarContent, SidebarFooter, SidebarGroup, SidebarGroupAction, SidebarGroupContent, SidebarGroupLabel, SidebarHeader, SidebarInset, SidebarMenu, SidebarMenuAction, SidebarMenuButton, SidebarMenuItem, SidebarProvider, SidebarTrigger, useSidebar } from "@/components/ui/sidebar";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useFeed } from "@/lib/feed";
import { useTheme } from "@/lib/theme";
import { isUnread } from "@/lib/unread";
import { StatusDot, UserAvatar, needsYou } from "./common";
import { DialogsProvider, useDialogs } from "./dialogs";
import { ErrorBoundary } from "./ErrorBoundary";

const byRecent = (a: ThreadSummary, b: ThreadSummary) => b.updatedAt.localeCompare(a.updatedAt);

function ThreadItem({ t, openId, owner }: { t: ThreadSummary; openId?: string; owner?: boolean }) {
  const seen = useSeen();
  const me = useMe().data?.user.login;
  const del = useDeleteThread();
  const navigate = useNavigate();
  const [confirm, setConfirm] = useState(false);
  const canDelete = !!me && (t.createdBy === me || owner || !t.projectId);
  const { isMobile, setOpenMobile } = useSidebar();
  const unread = isUnread(seen, t.id, t.updatedAt, openId);
  return (
    <SidebarMenuItem>
      <SidebarMenuButton asChild isActive={t.id === openId} tooltip={t.title}>
        <Link to="/t/$threadId" params={{ threadId: t.id }} onClick={() => isMobile && setOpenMobile(false)}>
          <StatusDot status={t.status} unread={unread} />
          <span className={unread ? "truncate font-semibold" : "truncate"}>{t.title || "(untitled)"}</span>
          {t.createdBy ? <UserAvatar login={t.createdBy} className="ml-auto size-4 text-[8px]" /> : null}
        </Link>
      </SidebarMenuButton>
      {canDelete ? (
        <SidebarMenuAction showOnHover onClick={() => setConfirm(true)} aria-label={`Delete ${t.title || "session"}`} title="Delete session">
          <Trash2 />
        </SidebarMenuAction>
      ) : null}
      <AlertDialog open={confirm} onOpenChange={setConfirm}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete “{t.title || "(untitled)"}”?</AlertDialogTitle>
            <AlertDialogDescription>The conversation and its workspace are removed for everyone.</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={() => del.mutate(t.id, { onSuccess: () => void (t.id === openId && navigate({ to: "/" })) })}>Delete</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </SidebarMenuItem>
  );
}

function ProjectGroup({ p, openId, openProject }: { p: OverviewProject; openId?: string; openProject?: string }) {
  const { newSession } = useDialogs();
  const threads = p.threads.filter((t) => t.kind !== "supervisor").sort(byRecent);
  const active = threads.filter((t) => needsYou(t.status)).length;
  const [open, setOpen] = useState(true);
  const memPending = useMemoryStats({ project: p.id }).data?.pending ?? 0;
  return (
    <Collapsible open={open} onOpenChange={setOpen}>
      <SidebarMenuItem>
        <CollapsibleTrigger asChild>
          <Button variant="ghost" size="icon" className="absolute left-0 top-1 size-6" aria-label={open ? `Collapse ${p.name}` : `Expand ${p.name}`}>
            <ChevronRight className={open ? "size-3.5 rotate-90 transition-transform" : "size-3.5 transition-transform"} />
          </Button>
        </CollapsibleTrigger>
        <SidebarMenuButton asChild isActive={openProject === p.id} className="pl-7">
          <Link to="/p/$projectId" params={{ projectId: p.id }} search={{ tab: undefined }}>
            <span className="truncate font-medium">{p.name}</span>
            {memPending ? (
              <span className="ml-auto inline-flex items-center gap-0.5 text-[10px] text-muted-foreground" title={`${memPending} memories to review`} aria-label={`${memPending} memories to review`}>
                <Brain className="size-3" />
                {memPending}
              </span>
            ) : null}
            {active ? <span className={`${memPending ? "" : "ml-auto "}rounded-full bg-warning px-1.5 text-[10px] text-black`}>{active}</span> : null}
          </Link>
        </SidebarMenuButton>
        {p.role !== "viewer" ? (
          <SidebarMenuAction onClick={() => newSession(p.id)} aria-label={`New session in ${p.name}`} title="New session">
            <Plus />
          </SidebarMenuAction>
        ) : null}
        <CollapsibleContent>
          <SidebarMenu className="mt-1 pl-3">
            {threads.length ? threads.map((t) => <ThreadItem key={t.id} t={t} openId={openId} owner={p.role === "owner"} />) : <li className="px-2 py-1 text-xs text-muted-foreground">No sessions yet</li>}
          </SidebarMenu>
        </CollapsibleContent>
      </SidebarMenuItem>
    </Collapsible>
  );
}

function UserMenu() {
  const me = useMe().data?.user;
  const { theme, setTheme } = useTheme();
  const { isMobile } = useSidebar();
  const [dev, setDev] = useState(getDevUser());
  const isAdmin = !!useMe().data?.admin;
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
        <SidebarMenuButton size="lg" aria-label="User menu" className="data-[state=open]:bg-sidebar-accent">
          <span className="relative shrink-0">
            <UserAvatar login={me.login} name={me.name} url={me.avatarUrl} className="size-8" />
            {pendingUsers ? <span className="absolute -right-0.5 -top-0.5 size-2.5 rounded-full bg-warning ring-2 ring-sidebar" aria-label={`${pendingUsers} users awaiting approval`} /> : null}
          </span>
          <div className="grid min-w-0 flex-1 text-left leading-tight">
            <span className="truncate text-sm font-medium">{me.name || me.login}</span>
            <span className="truncate text-xs text-muted-foreground">{me.email || me.login}</span>
          </div>
          <ChevronsUpDown className="ml-auto size-4 text-muted-foreground" />
        </SidebarMenuButton>
      </DropdownMenuTrigger>
      <DropdownMenuContent side={isMobile ? "top" : "right"} align="end" className="w-64">
        <div className="flex items-center gap-2 px-2 py-1.5">
          <UserAvatar login={me.login} name={me.name} url={me.avatarUrl} className="size-8" />
          <div className="grid min-w-0 leading-tight">
            <span className="truncate text-sm font-medium">{me.name || me.login}</span>
            <span className="truncate text-xs text-muted-foreground">@{me.login}{isAdmin ? " · admin" : ""}</span>
          </div>
        </div>
        <DropdownMenuSeparator />
        <DropdownMenuItem asChild>
          <Link to="/profile" search={{ tab: "profile" }}>
            <UserCog /> Profile
          </Link>
        </DropdownMenuItem>
        <DropdownMenuItem asChild>
          <Link to="/profile" search={{ tab: "preferences" }}>
            <SlidersHorizontal /> Preferences
          </Link>
        </DropdownMenuItem>
        <DropdownMenuItem asChild>
          <Link to="/memory">
            <Brain /> My memory
          </Link>
        </DropdownMenuItem>
        {isAdmin ? (
          <DropdownMenuItem asChild>
            <Link to="/admin">
              <Users /> Admin
              {pendingUsers ? <span className="ml-auto rounded-full bg-warning px-1.5 text-[10px] text-black">{pendingUsers} pending</span> : null}
            </Link>
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

function AppSidebar({ openId, openProject }: { openId?: string; openProject?: string }) {
  const overview = useOverview().data;
  const supervisor = useQuery(supervisorQuery).data;
  const { newSession, newProject, openPalette } = useDialogs();
  const { conn } = useFeed();
  const loc = useLocation();
  const personal = (overview?.personal ?? []).filter((t) => t.kind !== "supervisor").sort(byRecent);
  const svSummary = overview?.personal.find((t) => t.kind === "supervisor");
  const svStatus = svSummary?.status ?? supervisor?.status ?? "idle";
  const { isMobile, setOpenMobile } = useSidebar();
  const close = () => isMobile && setOpenMobile(false);
  return (
    <Sidebar>
      <SidebarHeader>
        <div className="flex items-center justify-between px-2 pt-1">
          <Link to="/" className="flex items-center gap-1.5 text-base font-semibold tracking-tight" onClick={close}>
            <Logo className="size-5" />
            har
          </Link>
          <div className="flex items-center gap-1">
            {conn === "offline" ? (
              <Tooltip>
                <TooltipTrigger asChild>
                  <WifiOff className="size-4 text-destructive" aria-label="Offline, reconnecting" />
                </TooltipTrigger>
                <TooltipContent>Live updates disconnected, reconnecting…</TooltipContent>
              </Tooltip>
            ) : null}
            <Button variant="ghost" size="icon" className="size-7" onClick={openPalette} aria-label="Search (Ctrl/Cmd+K)">
              <Search />
            </Button>
          </div>
        </div>
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton asChild isActive={loc.pathname === "/"}>
              <Link to="/" onClick={close}>
                <LayoutDashboard /> Dashboard
              </Link>
            </SidebarMenuButton>
          </SidebarMenuItem>
          <SidebarMenuItem>
            <SidebarMenuButton asChild isActive={loc.pathname === "/supervisor"}>
              <Link to="/supervisor" onClick={close}>
                <Bot /> My assistant
                <StatusDot status={svStatus} className="ml-auto" />
              </Link>
            </SidebarMenuButton>
          </SidebarMenuItem>
          {([["/references", "References", FileText], ["/connectors", "Connectors", Link2], ["/capabilities", "Capabilities", Plug]] as const).map(([to, label, Icon]) => (
            <SidebarMenuItem key={to}>
              <SidebarMenuButton asChild isActive={loc.pathname === to}>
                <Link to={to} onClick={close}>
                  <Icon /> {label}
                </Link>
              </SidebarMenuButton>
            </SidebarMenuItem>
          ))}
        </SidebarMenu>
      </SidebarHeader>
      <SidebarContent>
        <SidebarGroup>
          <SidebarGroupLabel>Projects</SidebarGroupLabel>
          <SidebarGroupAction onClick={newProject} aria-label="New project" title="New project">
            <Plus />
          </SidebarGroupAction>
          <SidebarGroupContent>
            <SidebarMenu>
              {overview?.projects.length ? (
                overview.projects.map((p) => <ProjectGroup key={p.id} p={p} openId={openId} openProject={openProject} />)
              ) : (
                <li className="px-2 py-1 text-xs text-muted-foreground">{overview ? "No projects yet." : "Loading…"}</li>
              )}
            </SidebarMenu>
          </SidebarGroupContent>
        </SidebarGroup>
        <SidebarGroup>
          <SidebarGroupLabel>Personal sessions</SidebarGroupLabel>
          <SidebarGroupAction onClick={() => newSession()} aria-label="New personal session" title="New session">
            <Plus />
          </SidebarGroupAction>
          <SidebarGroupContent>
            <SidebarMenu>
              {personal.length ? personal.map((t) => <ThreadItem key={t.id} t={t} openId={openId} />) : <li className="px-2 py-1 text-xs text-muted-foreground">{overview ? "No personal sessions." : ""}</li>}
            </SidebarMenu>
          </SidebarGroupContent>
        </SidebarGroup>
      </SidebarContent>
      <SidebarFooter>
        <SidebarMenu>
          <SidebarMenuItem>
            <UserMenu />
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarFooter>
    </Sidebar>
  );
}

function Palette({ open, setOpen }: { open: boolean; setOpen: (o: boolean) => void }) {
  const overview = useOverview().data;
  const navigate = useNavigate();
  const { newSession, newProject } = useDialogs();
  const { setTheme, resolvedTheme } = useTheme();
  const go = (fn: () => void) => () => {
    setOpen(false);
    fn();
  };
  const sessions = useMemo(() => [...(overview?.personal ?? []).map((t) => ({ t, project: "Personal" })), ...(overview?.projects ?? []).flatMap((p) => p.threads.map((t) => ({ t, project: p.name })))], [overview]);
  return (
    <CommandDialog open={open} onOpenChange={setOpen} title="Search" description="Jump to a session or project">
      <CommandInput placeholder="Jump to a session, project or action…" />
      <CommandList>
        <CommandEmpty>Nothing found.</CommandEmpty>
        <CommandGroup heading="Go to">
          <CommandItem onSelect={go(() => void navigate({ to: "/" }))}>Dashboard</CommandItem>
          <CommandItem onSelect={go(() => void navigate({ to: "/supervisor" }))}>My assistant</CommandItem>
          <CommandItem onSelect={go(() => void navigate({ to: "/memory" }))}>My memory</CommandItem>
          <CommandItem onSelect={go(() => void navigate({ to: "/references" }))}>References</CommandItem>
          <CommandItem onSelect={go(() => void navigate({ to: "/connectors" }))}>Connectors</CommandItem>
          <CommandItem onSelect={go(() => void navigate({ to: "/capabilities" }))}>Capabilities</CommandItem>
        </CommandGroup>
        <CommandGroup heading="Actions">
          <CommandItem onSelect={go(() => newSession())}>New session</CommandItem>
          <CommandItem onSelect={go(newProject)}>New project</CommandItem>
          <CommandItem onSelect={go(() => setTheme(resolvedTheme === "dark" ? "light" : "dark"))}>Toggle theme</CommandItem>
        </CommandGroup>
        {overview?.projects.length ? (
          <CommandGroup heading="Projects">
            {overview.projects.map((p) => (
              <CommandItem key={p.id} value={`project ${p.name}`} onSelect={go(() => void navigate({ to: "/p/$projectId", params: { projectId: p.id }, search: { tab: undefined } }))}>
                {p.name}
              </CommandItem>
            ))}
          </CommandGroup>
        ) : null}
        <CommandGroup heading="Sessions">
          {sessions
            .filter(({ t }) => t.kind !== "supervisor")
            .map(({ t, project }) => (
              <CommandItem key={t.id} value={`${t.title} ${project} ${t.id}`} onSelect={go(() => void navigate({ to: "/t/$threadId", params: { threadId: t.id } }))}>
                <StatusDot status={t.status} />
                {t.title || "(untitled)"}
                <span className="ml-auto text-xs text-muted-foreground">{project}</span>
              </CommandItem>
            ))}
        </CommandGroup>
      </CommandList>
    </CommandDialog>
  );
}

export function AppShell() {
  const [palette, setPalette] = useState(false);
  const params = useParams({ strict: false }) as { threadId?: string; projectId?: string };
  const loc = useLocation();
  const login = useMe().data?.user.login;
  // Usage statistics know a person by their login and nothing else; a no-op when statistics are off.
  useEffect(() => identifyUser(login), [login]);
  useEffect(() => {
    const h = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setPalette((p) => !p);
      }
    };
    window.addEventListener("keydown", h);
    return () => window.removeEventListener("keydown", h);
  }, []);
  return (
    <DialogsProvider openPalette={() => setPalette(true)}>
      <SidebarProvider className="h-svh">
        <AppSidebar openId={params.threadId} openProject={params.projectId} />
        <SidebarInset className="min-h-0 min-w-0">
          <header className="flex h-11 shrink-0 items-center gap-2 border-b px-3 md:hidden">
            <SidebarTrigger aria-label="Open navigation" />
            <Logo className="size-4" />
            <span className="text-sm font-medium">har</span>
          </header>
          <div className="min-h-0 flex-1 overflow-hidden">
            <ErrorBoundary reset={loc.pathname}>
              <Outlet />
            </ErrorBoundary>
          </div>
        </SidebarInset>
      </SidebarProvider>
      <Palette open={palette} setOpen={setPalette} />
    </DialogsProvider>
  );
}
