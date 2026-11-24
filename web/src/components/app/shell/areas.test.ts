import { describe, expect, it } from "vitest";
import { AREAS, ME_AREA, PROJECT_AREAS, PROJECT_TAB_IDS, areaHref, locatePath, locateTab, rememberView, viewHref, viewTitle, visibleViews } from "./areas";
import { pageTitle, tabFor } from "./tabs";

// every project tab id that existed before the areas did
const OLD_TABS = ["board", "overview", "requirements", "tasks", "workflows", "insights", "roles", "schedules", "recipes", "memory", "members", "capabilities", "audit", "usage", "notifications", "settings"];
// every personal route that existed before the Me area did
const OLD_PATHS = ["/references", "/connectors", "/memory", "/roles", "/schedules", "/capabilities", "/insights", "/usage"];

describe("areas registry", () => {
  it("resolves every old project tab id to exactly one area and view", () => {
    for (const id of OLD_TABS) {
      const hits = PROJECT_AREAS.flatMap((a) => a.views.filter((v) => v.id === id).map((v) => ({ a, v })));
      expect(hits, id).toHaveLength(1);
      expect(locateTab(id)?.view.id).toBe(id);
    }
    expect(locateTab(undefined)?.view.id).toBe("board");
    expect([...PROJECT_TAB_IDS].sort()).toEqual([...OLD_TABS].sort());
  });

  it("resolves every old personal route to the Me area", () => {
    for (const p of OLD_PATHS) {
      expect(locatePath(p)?.area.id, p).toBe("me");
      expect(ME_AREA.views.filter((v) => v.id === p)).toHaveLength(1);
    }
  });

  it("has no area without views, no duplicated area, and a shown view in each", () => {
    expect(new Set(AREAS.map((a) => a.id)).size).toBe(AREAS.length);
    for (const a of AREAS) {
      expect(a.views.length, a.id).toBeGreaterThan(0);
      expect(visibleViews(a, { owner: false }).length, a.id).toBeGreaterThan(0);
      expect(new Set(a.views.map((v) => v.id)).size).toBe(a.views.length);
    }
  });

  it("is the short set: four project areas and Me", () => {
    expect(PROJECT_AREAS.map((a) => a.label)).toEqual(["Work", "Automation", "Insights", "Settings"]);
    expect(ME_AREA.label).toBe("Me");
  });

  it("titles a view 'Area · View' without repeating a name", () => {
    expect(viewTitle(locateTab("board")!)).toBe("Work · Board");
    expect(viewTitle(locateTab("audit")!)).toBe("Insights · Activity");
    expect(pageTitle("/references")).toBe("Me · Library");
  });

  it("gives all the views of an area one editor tab", () => {
    expect(tabFor("/p/p1", { tab: "members" })?.key).toBe(tabFor("/p/p1", { tab: "roles" })?.key);
    expect(tabFor("/p/p1", { tab: "usage" })?.key).toBe(tabFor("/p/p1", {tab: "audit"})?.key);
    expect(tabFor("/p/p1", { tab: "board" })?.key).not.toBe(tabFor("/p/p1", { tab: "workflows" })?.key);
    expect(tabFor("/p/p1", { tab: "members" })?.href).toBe("/p/p1?tab=members");
    expect(tabFor("/connectors", {})?.key).toBe(tabFor("/memory", {})?.key);
    expect(tabFor("/jobs", {})?.key).toBe("page:/jobs");
  });

  it("hides the owner-only view from everyone else", () => {
    const settings = AREAS.find((a) => a.id === "settings")!;
    expect(visibleViews(settings, { owner: true }).map((v) => v.label)[0]).toBe("General");
    expect(visibleViews(settings, { owner: false }).map((v) => v.label)).not.toContain("General");
  });

  it("opens an area on the view last shown there, per project", () => {
    const settings = AREAS.find((a) => a.id === "settings")!;
    expect(areaHref(settings, "px", true)).toBe("/p/px?tab=settings");
    expect(areaHref(settings, "px", false)).toBe("/p/px?tab=members");
    rememberView(settings, "notifications", "px");
    expect(areaHref(settings, "px", true)).toBe("/p/px?tab=notifications");
    expect(areaHref(settings, "py", true)).toBe("/p/py?tab=settings");
    expect(viewHref(ME_AREA, ME_AREA.views[0]!)).toBe("/references");
  });
});
