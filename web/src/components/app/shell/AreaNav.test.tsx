import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it } from "vitest";
import { mockFetch, reply } from "@/test/fetch";
import { me, overview, project, shellRoutes } from "@/test/fixtures";
import { renderApp } from "@/test/render";

const routes = () => {
  const p = project("p1", { name: "payments" });
  return {
    ...shellRoutes(overview({ projects: [{ ...p, threads: [] }] }), me({ projects: [p] })),
    "GET /api/projects/p1": p,
    "GET /api/projects/p1/members": [],
    "GET /api/projects/p1/github": reply(404),
    "GET /api/memory/stats": reply(404),
    "GET /api/memory": { entries: [], enabled: true },
    "GET /api/projects/p1/board": { columns: [] },
  };
};

beforeEach(() => localStorage.clear());

describe("area sub-navigation", () => {
  it("is a labelled tablist with the shown view selected and one tab stop", async () => {
    mockFetch(routes());
    await renderApp("/p/p1?tab=members");
    const list = await screen.findByRole("tablist", { name: "Settings views" });
    const tabs = within(list).getAllByRole("tab");
    expect(tabs.map((t) => t.textContent)).toEqual(["General", "Members & access", "Roles & executors", "Tools & skills", "Memory & docs", "Notifications"]);
    expect(tabs.filter((t) => t.getAttribute("aria-selected") === "true").map((t) => t.textContent)).toEqual(["Members & access"]);
    expect(tabs.filter((t) => t.tabIndex === 0)).toHaveLength(1);
    const panel = screen.getAllByRole("tabpanel").find((p) => p.getAttribute("aria-labelledby") === tabs[1]!.id);
    expect(panel).toBeTruthy();
  });

  it("moves with the arrow keys, Home and End, and shows the view it lands on", async () => {
    mockFetch(routes());
    const { router } = await renderApp("/p/p1?tab=members");
    const first = await screen.findByRole("tab", { name: "Members & access", selected: true });
    fireEvent.keyDown(first, { key: "ArrowRight" });
    await waitFor(() => expect(router.state.location.search).toEqual({ tab: "roles" }));
    expect(await screen.findByRole("tab", { name: "Roles & executors", selected: true })).toBeTruthy();
    fireEvent.keyDown(screen.getByRole("tab", { name: "Roles & executors" }), { key: "ArrowLeft" });
    await waitFor(() => expect(router.state.location.search).toEqual({ tab: "members" }));
    fireEvent.keyDown(screen.getByRole("tab", { name: "Members & access" }), { key: "End" });
    await waitFor(() => expect(router.state.location.search).toEqual({ tab: "notifications" }));
    fireEvent.keyDown(screen.getByRole("tab", { name: "Notifications" }), { key: "ArrowRight" }); // wraps to the first
    await waitFor(() => expect(router.state.location.search).toEqual({ tab: "settings" }));
    fireEvent.keyDown(screen.getByRole("tab", { name: "General" }), { key: "Home" });
    expect(router.state.location.search).toEqual({ tab: "settings" });
  });

  it("remembers the view last shown in an area for the explorer link", async () => {
    mockFetch(routes());
    await renderApp("/p/p1?tab=roles");
    await screen.findByRole("tab", { name: "Roles & executors", selected: true });
    await waitFor(() => expect(screen.getAllByRole("link", { name: "Settings" })[0]!.getAttribute("href")).toBe("/#/p/p1?tab=roles"));
  });

  it("is one editor tab per area, titled with the area and the view", async () => {
    mockFetch(routes());
    await renderApp("/p/p1?tab=insights");
    const strip = within(await screen.findByRole("tablist", { name: "Open editors" }));
    expect(await strip.findByRole("tab", { name: "Insights · Overview", selected: true })).toBeTruthy();
    fireEvent.click(await screen.findByRole("tab", { name: "Activity" }));
    expect(await strip.findByRole("tab", { name: "Insights · Activity", selected: true })).toBeTruthy();
    expect(strip.getAllByRole("tab")).toHaveLength(1);
  });

  it("opens a deep link to any old tab id in its area, and highlights the area in the explorer", async () => {
    mockFetch(routes());
    await renderApp("/p/p1?tab=audit");
    expect(await screen.findByRole("tab", { name: "Activity", selected: true })).toBeTruthy();
    const nav = within(screen.getByRole("navigation", { name: "Explorer" }));
    expect(nav.getByRole("link", { name: "Insights" }).getAttribute("aria-current")).toBe("page");
    expect(nav.getByRole("link", { name: "Work" }).getAttribute("aria-current")).toBeNull();
  });

  it("serves the Me area from the old personal routes", async () => {
    mockFetch({ ...shellRoutes(), "GET /api/me/insights": { projectId: undefined } });
    const { router } = await renderApp("/connectors");
    const list = await screen.findByRole("tablist", { name: "Me views" });
    expect(within(list).getAllByRole("tab").map((t) => t.textContent)).toContain("Library");
    expect(within(list).getByRole("tab", { name: "Connectors", selected: true })).toBeTruthy();
    fireEvent.click(within(list).getByRole("tab", { name: "My roles" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/roles"));
  });
});
