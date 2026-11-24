// The editor's open tabs. The URL stays the source of truth for what is shown: every route keeps working, and the tab strip is the
// list of places visited (and kept) in this browser. Opening something navigates; a location the strip does not know yet becomes a tab.
import { useLocation, useRouter } from "@tanstack/react-router";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { readJSON, writeJSON } from "@/lib/persist";
import { locatePath, locateTab, viewTitle } from "./areas";

export type TabKind = "home" | "session" | "project" | "requirement" | "task" | "workflow" | "run" | "page";

export interface EditorTab {
  /** Stable identity of a place: the same place always focuses the same tab. */
  key: string;
  /** The route (path and search) the tab shows, in the shape the router takes as `href`. */
  href: string;
  kind: TabKind;
  projectId?: string;
  /** thread id, task id, workflow id or run id */
  id?: string;
  /** the project tab ("board", "members" ...) of a project place */
  section?: string;
}

const PAGES: Record<string, string> = {
  "/": "Dashboard",
  "/supervisor": "My assistant",
  "/inbox": "Needs you",
  "/jobs": "Job search",
  "/references": "Me",
  "/connectors": "Me",
  "/capabilities": "Me",
  "/schedules": "Me",
  "/roles": "Me",
  "/usage": "Me",
  "/insights": "Me",
  "/memory": "Me",
  "/profile": "Settings",
  "/admin": "Admin",
  "/help": "Help",
};

export const pageTitle = (path: string) => {
  const l = locatePath(path);
  return l ? viewTitle(l) : PAGES[path];
};

/** The words on a project place's tab: "Work · Board". */
export function projectTitle(section: string | undefined): string {
  const l = locateTab(section);
  return l ? viewTitle(l) : (section ?? "");
}

/** The href of a project place. */
export function projectHref(projectId: string, section?: string, extra: { task?: string; wf?: string; run?: string; req?: string } = {}) {
  const q = new URLSearchParams();
  if (section) q.set("tab", section);
  for (const [k, v] of Object.entries(extra)) if (v) q.set(k, v);
  const s = q.toString();
  return `/p/${projectId}${s ? `?${s}` : ""}`;
}

/** The tab a location belongs to, or null for a place that is not a tab (not found). */
export function tabFor(pathname: string, search: Record<string, unknown>): EditorTab | null {
  const str = (v: unknown) => (typeof v === "string" && v ? v : undefined);
  const thread = /^\/t\/([^/]+)$/.exec(pathname);
  if (thread) return { key: `t:${thread[1]!}`, href: `/t/${thread[1]!}`, kind: "session", id: thread[1]! };
  const proj = /^\/p\/([^/]+)$/.exec(pathname);
  if (proj) {
    const projectId = proj[1]!;
    const section = str(search.tab) ?? "board";
    const task = str(search.task);
    const wf = str(search.wf);
    const run = str(search.run);
    const req = str(search.req);
    if (section === "requirements" && req) return { key: `p:${projectId}:req:${req}`, href: projectHref(projectId, "requirements", { req }), kind: "requirement", projectId, id: req, section };
    if (section === "tasks" && task) return { key: `p:${projectId}:task:${task}`, href: projectHref(projectId, "tasks", { task }), kind: "task", projectId, id: task, section };
    if (section === "workflows" && run) return { key: `p:${projectId}:run:${run}`, href: projectHref(projectId, "workflows", { run }), kind: "run", projectId, id: run, section };
    if (section === "workflows" && wf) return { key: `p:${projectId}:wf:${wf}`, href: projectHref(projectId, "workflows", { wf }), kind: "workflow", projectId, id: wf, section };
    // One tab per area: the view shown is the tab's address, not its identity.
    const area = locateTab(section)?.area.id ?? section;
    return { key: `p:${projectId}:${area}`, href: projectHref(projectId, str(search.tab)), kind: "project", projectId, section };
  }
  if (pathname === "/") return { key: "home", href: "/", kind: "home" };
  if (locatePath(pathname)) return { key: "page:me", href: pathname, kind: "page" };
  if (PAGES[pathname]) return { key: `page:${pathname}`, href: pathname, kind: "page" };
  return null;
}

const storageKey = (login: string) => `7ar.tabs.v1.${login}`;

/** A strip stored before the areas existed holds one tab per view: fold them into one tab per area. */
function normalize(list: EditorTab[]): EditorTab[] {
  const out: EditorTab[] = [];
  for (const t of list) {
    let n = t;
    if (t.kind === "project" || (t.kind === "page" && locatePath(t.href))) {
      const [path = "", query = ""] = t.href.split("?");
      n = tabFor(path, Object.fromEntries(new URLSearchParams(query))) ?? t;
    }
    if (!out.some((o) => o.key === n.key)) out.push(n);
  }
  return out;
}

const valid = (t: unknown): t is EditorTab => !!t && typeof (t as EditorTab).key === "string" && typeof (t as EditorTab).href === "string" && typeof (t as EditorTab).kind === "string";

export interface Tabs {
  tabs: EditorTab[];
  active?: EditorTab;
  open: (href: string) => void;
  close: (key: string) => void;
  closeOthers: (key: string) => void;
  move: (from: string, to: string) => void;
  next: (dir: 1 | -1) => void;
}

export function useEditorTabs(login: string): Tabs {
  const router = useRouter();
  const pathname = useLocation({ select: (l) => l.pathname });
  const search = useLocation({ select: (l) => l.search }) as unknown as Record<string, unknown>;
  const current = useMemo(() => tabFor(pathname, search), [pathname, search]);
  const [stored, setStored] = useState<EditorTab[]>(() => normalize(readJSON<unknown[]>(storageKey(login), []).filter(valid)));
  const loaded = useRef(login);
  // The user known after the first render (the shell mounts once /me answered, so this is rare): load their strip.
  useEffect(() => {
    if (loaded.current !== login) {
      loaded.current = login;
      setStored(normalize(readJSON<unknown[]>(storageKey(login), []).filter(valid)));
    }
  }, [login]);

  const currentKey = current?.key;
  const currentHref = current?.href;
  // A new place becomes a tab; a known place keeps its slot (its href follows, e.g. a project tab's search).
  useEffect(() => {
    if (!current) return;
    setStored((l) => {
      const i = l.findIndex((t) => t.key === current.key);
      if (i < 0) return [...l, current];
      return l[i]!.href === current.href ? l : l.map((t, j) => (j === i ? current : t));
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [currentKey, currentHref]);

  useEffect(() => {
    writeJSON(storageKey(login), stored);
  }, [login, stored]);

  const tabs = useMemo(() => (current && !stored.some((t) => t.key === current.key) ? [...stored, current] : stored), [stored, current]);
  const tabsRef = useRef(tabs);
  tabsRef.current = tabs;

  const open = useCallback((href: string) => void router.navigate({ href }), [router]);
  const goAfterClose = useCallback(
    (remaining: EditorTab[], idx: number) => {
      const target = remaining[idx] ?? remaining[idx - 1];
      void router.navigate({ href: target?.href ?? "/" });
    },
    [router],
  );
  const close = useCallback(
    (key: string) => {
      const list = tabsRef.current;
      const idx = list.findIndex((t) => t.key === key);
      if (idx < 0) return;
      const remaining = list.filter((t) => t.key !== key);
      setStored(remaining);
      if (current?.key === key) goAfterClose(remaining, idx);
    },
    [current?.key, goAfterClose],
  );
  const closeOthers = useCallback(
    (key: string) => {
      const keep = tabsRef.current.filter((t) => t.key === key);
      setStored(keep);
      if (current?.key !== key && keep[0]) void router.navigate({ href: keep[0].href });
    },
    [current?.key, router],
  );
  const move = useCallback((from: string, to: string) => {
    setStored(() => {
      const list = [...tabsRef.current];
      const a = list.findIndex((t) => t.key === from);
      const b = list.findIndex((t) => t.key === to);
      if (a < 0 || b < 0 || a === b) return list;
      const [t] = list.splice(a, 1) as [EditorTab];
      list.splice(b, 0, t);
      return list;
    });
  }, []);
  const next = useCallback(
    (dir: 1 | -1) => {
      const list = tabsRef.current;
      if (list.length < 2) return;
      const i = list.findIndex((t) => t.key === current?.key);
      const target = list[(i + dir + list.length) % list.length]!;
      void router.navigate({ href: target.href });
    },
    [current?.key, router],
  );
  return { tabs, active: current ?? undefined, open, close, closeOthers, move, next };
}
