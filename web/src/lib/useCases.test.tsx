import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { mockFetch, reply } from "@/test/fetch";
import { me, overview, project, shellRoutes, thread } from "@/test/fixtures";
import { renderApp } from "@/test/render";
import { USE_CASES } from "./useCases";

const jobs = { profile: { enabled: false, mode: "queue", titles: [], minScore: 4.5, queueScore: 4, dailyCap: 10, companies: [] }, counts: {}, jobs: [], questions: [] };
const withProject = () => {
  const p = project("pr", { name: "payments" });
  return shellRoutes(overview({ projects: [{ ...p, threads: [thread("t1")] }] }), me({ projects: [p] }));
};

describe("use cases", () => {
  it("are a data list: each has an id, title, one line, route and the capability it needs", () => {
    expect(new Set(USE_CASES.map((u) => u.id)).size).toBe(USE_CASES.length);
    for (const u of USE_CASES) {
      expect(u.title && u.blurb && u.command).toBeTruthy();
      expect(u.route.startsWith("/")).toBe(true);
    }
    expect(USE_CASES.find((u) => u.id === "job-search")?.route).toBe("/jobs");
  });

  it("show on the Dashboard when the server has job search, and Job search is not in the explorer", async () => {
    mockFetch({ ...withProject(), "GET /api/me/jobs": jobs });
    await renderApp("/");
    const region = within(await screen.findByRole("region", { name: "Use cases" }));
    const card = region.getByRole("link", { name: /Job search assistant/ });
    expect(card.textContent).toContain("Finds and tracks openings from your CV");
    expect(card.getAttribute("href")).toBe("/#/jobs");
    expect(within(screen.getByRole("navigation", { name: "Explorer" })).queryByRole("link", { name: /Job/ })).toBeNull();
  });

  it("are hidden when the server has no job search", async () => {
    mockFetch({ ...withProject(), "GET /api/me/jobs": reply(404) });
    await renderApp("/");
    await screen.findByRole("heading", { name: "Dashboard" });
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.queryByRole("region", { name: "Use cases" })).toBeNull();
  });

  it("also show next to 'Try it with a sample' on the empty Dashboard", async () => {
    mockFetch({ ...shellRoutes(), "GET /api/me/jobs": jobs });
    await renderApp("/");
    expect(await screen.findByRole("button", { name: /Try it with a sample/ })).toBeTruthy();
    expect(await screen.findByRole("region", { name: "Use cases" })).toBeTruthy();
  });

  it("have a palette entry only when available", async () => {
    const m = mockFetch({ ...withProject(), "GET /api/me/jobs": jobs });
    const a = await renderApp("/");
    await screen.findByRole("region", { name: "Use cases" });
    fireEvent.keyDown(window, { key: "k", ctrlKey: true });
    const item = await within(await screen.findByRole("listbox")).findByText("Use case: Job search");
    fireEvent.click(item);
    await waitFor(() => expect(a.router.state.location.pathname).toBe("/jobs"));
    expect(m.find("GET", "/api/me/jobs").length).toBeGreaterThan(0);
    a.unmount();

    mockFetch({ ...withProject(), "GET /api/me/jobs": reply(501) });
    await renderApp("/");
    fireEvent.keyDown(window, { key: "k", ctrlKey: true });
    const list = within(await screen.findByRole("listbox"));
    await list.findByText("Open help");
    expect(list.queryByText("Use case: Job search")).toBeNull();
  });
});

describe("palette finds every view by name", () => {
  it.each([["Members", "Settings · Members & access"], ["Usage", "Insights · Usage & caps"], ["Audit", "Insights · Activity"], ["Notifications", "Settings · Notifications"], ["Recipes", "Automation · Recipes"], ["References", "Me · Library"]])("%s", async (q, label) => {
    mockFetch(withProject());
    await renderApp("/p/pr");
    fireEvent.keyDown(window, { key: "k", ctrlKey: true });
    const input = await screen.findByPlaceholderText(/Jump to a session/);
    fireEvent.change(input, { target: { value: q } });
    const list = within(await screen.findByRole("listbox"));
    expect((await list.findAllByText(label)).length).toBeGreaterThan(0);
  });
});

describe("explorer", () => {
  it("lists the short set of places for a project", async () => {
    mockFetch(withProject());
    await renderApp("/p/pr");
    const nav = within(await screen.findByRole("navigation", { name: "Explorer" }));
    await nav.findByRole("link", { name: "Work" });
    const names = nav.getAllByRole("link").map((a) => a.textContent?.replace(/\s+/g, " ").trim());
    // places first, sessions are listed too
    expect(names.filter((n) => n && !/^Session /.test(n))).toEqual(["Dashboard", "Needs you", "Work", "Automation", "Insights", "Settings", "My assistant", "Me"]);
    // one Settings (the project's); the account link under the list has its own name
    expect(screen.getAllByRole("link", { name: "Settings" })).toHaveLength(1);
    expect(screen.getAllByRole("link", { name: "Account" })).toHaveLength(1);
  });
});
