// The map of the interface: a handful of areas, each with a few views. The explorer lists areas, the editor shows one tab per area and
// the page chrome draws the views as a sub-navigation. Every old tab id (?tab=members) and every old personal route (/references) is
// the id of exactly one view, so deep links, notifications and help links keep working unchanged.
import { readJSON, writeJSON } from "@/lib/persist";

export type AreaScope = "project" | "me";

export interface AreaView {
  /** A project tab id (?tab=...) for the project areas; the route path (/references) for the Me area. */
  id: string;
  label: string;
  /** Other words people look for (the palette finds the view by them). */
  aliases?: string[];
  /** Reachable and highlighted when shown, but not drawn as a pill. */
  hidden?: boolean;
  /** Only the project's owners have it. */
  ownerOnly?: boolean;
}

export interface Area {
  id: string;
  label: string;
  scope: AreaScope;
  hint: string;
  views: AreaView[];
}

export const AREAS: Area[] = [
  {
    id: "work",
    label: "Work",
    scope: "project",
    hint: "Sessions and tasks as cards, the goals you write down, and the work handed to agents",
    views: [
      { id: "board", label: "Board" },
      { id: "requirements", label: "Requirements" },
      { id: "tasks", label: "Tasks", aliases: ["triggers"] },
      { id: "overview", label: "Sessions", hidden: true },
    ],
  },
  {
    id: "automation",
    label: "Automation",
    scope: "project",
    hint: "Workflows, schedules and ready-made recipes",
    views: [
      { id: "workflows", label: "Workflows", aliases: ["runs"] },
      { id: "schedules", label: "Schedules" },
      { id: "recipes", label: "Recipes" },
    ],
  },
  {
    id: "insights",
    label: "Insights",
    scope: "project",
    hint: "What the agents delivered, what it cost, and who did what",
    views: [
      { id: "insights", label: "Overview" },
      { id: "usage", label: "Usage & caps", aliases: ["budget", "cap", "cost"] },
      { id: "audit", label: "Activity", aliases: ["audit log"] },
    ],
  },
  {
    id: "settings",
    label: "Settings",
    scope: "project",
    hint: "The project itself, its people, roles, tools, memory and notifications",
    views: [
      { id: "settings", label: "General", aliases: ["project settings", "repository"], ownerOnly: true },
      { id: "members", label: "Members & access", aliases: ["people", "invite"] },
      { id: "roles", label: "Roles & executors", aliases: ["executor"] },
      { id: "capabilities", label: "Tools & skills", aliases: ["mcp"] },
      { id: "memory", label: "Memory & docs", aliases: ["documents"] },
      { id: "notifications", label: "Notifications", aliases: ["slack", "email"] },
    ],
  },
  {
    id: "me",
    label: "Me",
    scope: "me",
    hint: "Your own library, connectors, memory, roles, schedules, tools and numbers, in every project",
    views: [
      { id: "/references", label: "Library", aliases: ["references", "documents"] },
      { id: "/connectors", label: "Connectors" },
      { id: "/memory", label: "Memory", aliases: ["my memory"] },
      { id: "/roles", label: "My roles", aliases: ["roles"] },
      { id: "/schedules", label: "My schedules", aliases: ["schedules"] },
      { id: "/capabilities", label: "My tools & skills", aliases: ["tools", "skills"] },
      { id: "/insights", label: "My insights", aliases: ["insights"] },
      { id: "/usage", label: "My usage", aliases: ["usage", "caps"] },
    ],
  },
];

export const PROJECT_AREAS = AREAS.filter((a) => a.scope === "project");
export const ME_AREA = AREAS.find((a) => a.scope === "me")!;

export interface Located {
  area: Area;
  view: AreaView;
}

const byId = new Map<string, Located>();
for (const area of AREAS) for (const view of area.views) byId.set(`${area.scope}:${view.id}`, { area, view });

/** The area and view of a project tab id (no id: the board). */
export function locateTab(tab: string | undefined): Located | undefined {
  return byId.get(`project:${tab ?? "board"}`);
}

/** The area and view of a personal route, or undefined when the path is not part of the Me area. */
export function locatePath(path: string): Located | undefined {
  return byId.get(`me:${path}`);
}

export const isMePath = (path: string) => !!locatePath(path);

/** Every project tab id there is. */
export const PROJECT_TAB_IDS = PROJECT_AREAS.flatMap((a) => a.views.map((v) => v.id));

/** "Work · Board": the area, then the view; a view that is called like its area says it once. */
export function viewTitle(l: Located): string {
  return l.view.label === l.area.label ? l.area.label : `${l.area.label} · ${l.view.label}`;
}

/** The views drawn as pills for a person: no hidden ones (unless shown), no owner-only ones for non-owners. */
export function visibleViews(area: Area, opts: { owner?: boolean; current?: string } = {}): AreaView[] {
  return area.views.filter((v) => (!v.hidden || v.id === opts.current) && (!v.ownerOnly || opts.owner));
}

const memoryKey = (scope: string) => `7ar.ui.view.${scope}`;

/** The view last shown in an area (per project; the Me area has its own scope). */
export function rememberedView(area: Area, projectId?: string): string | undefined {
  const all = readJSON<Record<string, string>>(memoryKey(area.scope === "me" ? "me" : (projectId ?? "")), {});
  const id = all[area.id];
  return id && area.views.some((v) => v.id === id) ? id : undefined;
}

export function rememberView(area: Area, viewId: string, projectId?: string) {
  const key = memoryKey(area.scope === "me" ? "me" : (projectId ?? ""));
  const all = readJSON<Record<string, string>>(key, {});
  if (all[area.id] === viewId) return;
  writeJSON(key, { ...all, [area.id]: viewId });
}

/** The address of a view. */
export function viewHref(area: Area, view: AreaView, projectId?: string): string {
  if (area.scope === "me") return view.id;
  return `/p/${projectId ?? ""}?tab=${view.id}`;
}

/** Where a click on the area itself goes: the view last shown there, else its first. */
export function areaHref(area: Area, projectId?: string, owner = true): string {
  const views = visibleViews(area, { owner });
  const id = rememberedView(area, projectId);
  const view = views.find((v) => v.id === id) ?? views[0]!;
  return viewHref(area, view, projectId);
}
