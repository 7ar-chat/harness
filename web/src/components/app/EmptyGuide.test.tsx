import { screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { mockFetch, reply } from "@/test/fetch";
import { me, overview, project, shellRoutes } from "@/test/fixtures";
import { renderApp } from "@/test/render";

const page = () => within(screen.getByRole("main"));
const p = project("p1", { name: "payments" });
const routes = () => ({
  ...shellRoutes(overview({ projects: [{ ...p, threads: [] }] }), me({ projects: [p] })),
  "GET /api/projects/p1": p,
  "GET /api/memory/stats": reply(404),
});

describe("empty states that teach", () => {
  it("the Tasks tab says what tasks are, offers the first one and links to help", async () => {
    mockFetch({ ...routes(), "GET /api/projects/p1/tasks?limit=500": [], "GET /api/projects/p1/triggers": [] });
    await renderApp("/p/p1?tab=tasks");
    const guide = await page().findByRole("region", { name: "No tasks yet" });
    expect(within(guide).getByText(/to put an agent to work unattended/)).toBeTruthy();
    expect(within(guide).getByRole("button", { name: "Create your first task" })).toBeTruthy();
    expect(within(guide).getByRole("link", { name: "Learn more" }).getAttribute("href")).toContain("/help?a=hand-off-a-task");
  });

  it("the Schedules tab teaches the idea of a recurring job", async () => {
    mockFetch({ ...routes(), "GET /api/projects/p1/schedules": [] });
    await renderApp("/p/p1?tab=schedules");
    const guide = await page().findByRole("region", { name: "No schedules yet" });
    expect(within(guide).getByRole("button", { name: "Create your first schedule" })).toBeTruthy();
  });

  it("the inbox explains what shows up there", async () => {
    mockFetch({ ...shellRoutes(), "GET /api/needs-review": { tasks: [], threads: [], gates: [], count: 0 } });
    await renderApp("/inbox");
    expect(await page().findByRole("region", { name: "Nothing waits for you" })).toBeTruthy();
  });
});
