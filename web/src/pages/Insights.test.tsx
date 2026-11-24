import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { InsightsView } from "@/api/insights";
import { mockFetch, reply } from "@/test/fetch";
import { me, overview, project, shellRoutes } from "@/test/fixtures";
import { renderApp } from "@/test/render";
import { duration, money } from "./Insights";

const ref = (id: string, title: string, value: number, detail = "") => ({ id, title, state: "done", value, detail });
const view = (over: Partial<InsightsView> = {}): InsightsView =>
  ({
    projectId: "p1", from: "2026-10-26T00:00:00Z", to: "2026-11-25T00:00:00Z", days: 30, tz: "UTC", generatedAt: "2026-11-24T10:00:00Z",
    summary: "In the last 30 days your agents merged 18 PRs (82% first-pass), cost $41.20 ($2.29 per PR), median cycle 3h 10m; 4 tasks needed a human for more than a day.",
    empty: false,
    delivery: {
      started: 20, merged: 18, completed: 0, failed: 1, cancelled: 0, blocked: 1, mergeRate: 0.9, firstPassRate: 0.82, cycleMedianSec: 11400, cycleP90Sec: 40000, waitMedianSec: 3000,
      neededHumanOverDay: 4, reworkPerPr: 0.4, reworkSpread: [14, 3, 1, 0], ci: { passed: 20, failed: 3 }, workingSec: 70000, waitingSec: 30000,
      cycleBuckets: [{ label: "under 10 min", count: 1 }, { label: "1 to 4 h", count: 12 }, { label: "over 3 days", count: 5 }], byRole: [], byExecutor: [],
    },
    flow: { requirementsOpened: 3, requirementsDone: 2, weeks: [{ week: "2026-11-02", merged: 5, started: 6 }, { week: "2026-11-09", merged: 13, started: 14 }] },
    cost: {
      totalUsd: 41.2, perMergedUsd: 2.29, unpricedRuns: 0, runs: 30, inputTokens: 1, outputTokens: 1, cacheReadTokens: 1, cacheWriteTokens: 1,
      models: [{ model: "gpt-x", runs: 30, inputTokens: 1000, outputTokens: 10, cacheReadTokens: 600, cacheWriteTokens: 50, costUsd: 41.2 }],
      byDay: [{ day: "2026-11-02", usd: 20, byModel: { "gpt-x": 20 } }, { day: "2026-11-03", usd: 21.2, byModel: { "gpt-x": 21.2 } }],
      byRole: [], byExecutor: [], topTasks: [],
    },
    quality: { reviewApproved: 1, reviewBlocked: 0, reviewBlockRate: 0, policyDenied: 0, policyAsked: 0, loopGuardTrips: 0, escalations: 0 },
    people: [],
    workflows: { runs: 0, done: 0, failed: 0, successRate: 0, gateWaits: 0, gateWaitMedianSec: 0, gateWaitTotalSec: 0 },
    attention: {
      slowest: [ref("t1", "Migrate the billing job", 90000)], blockedLongest: [ref("t2", "Fix the flaky test", 172800, "waiting 2d")],
      mostExpensive: [ref("t3", "Rewrite the parser", 9.5)], mostReworked: [],
    },
    tasks: [],
    ...over,
  }) as InsightsView;

const page = () => within(screen.getByRole("main"));
const routes = (v: InsightsView) => ({
  ...shellRoutes(overview(), me({ projects: [project("p1")] })),
  "GET /api/projects/p1": project("p1"),
  "GET /api/projects/p1/insights": v,
});

describe("Insights tab", () => {
  it("answers in plain words: the summary, four headline numbers, the charts and what needs attention", async () => {
    const m = mockFetch(routes(view()));
    await renderApp("/p/p1?tab=insights");

    expect(await page().findByTestId("insights-summary")).toHaveProperty("textContent", expect.stringContaining("merged 18 PRs (82% first-pass)"));
    for (const label of ["Merged PRs", "First-pass success", "Cost per PR", "Time to merge"]) expect(page().getAllByText(label).length).toBeGreaterThan(0);
    expect(page().getByText("82%")).toBeTruthy();
    expect(page().getByText("$2.29")).toBeTruthy();
    expect(page().getByText("3h 10m")).toBeTruthy();
    expect(page().getByRole("img", { name: /Merged PRs per week, 2 weeks, 18 in total/ })).toBeTruthy();
    expect(page().getByRole("img", { name: /Cost per day, 2 days/ })).toBeTruthy();
    expect(page().getByRole("img", { name: /Working 70%, waiting on a person 30%/ })).toBeTruthy();
    expect(page().getByRole("link", { name: "Fix the flaky test" })).toBeTruthy();
    expect(page().getByText("No task needed rework.")).toBeTruthy();
    expect(m.calls.some((c) => c.url.startsWith("/api/projects/p1/insights?range=30d&tz="))).toBe(true);
  });

  it("changes the period", async () => {
    const m = mockFetch(routes(view()));
    await renderApp("/p/p1?tab=insights");
    await page().findByTestId("insights-summary");

    fireEvent.click(page().getByRole("button", { name: "7 days" }));

    await waitFor(() => expect(m.calls.some((c) => c.url.startsWith("/api/projects/p1/insights?range=7d"))).toBe(true));
  });

  it("explains an empty project and points at handing off a first task", async () => {
    mockFetch(routes(view({ empty: true })));
    await renderApp("/p/p1?tab=insights");

    expect(await page().findByText("Insights appear after the first merged task")).toBeTruthy();
    expect(page().getByRole("link", { name: "Hand off a first task" }).getAttribute("href")).toContain("tab=tasks");
  });

  it("says so when the server refuses", async () => {
    mockFetch({ ...routes(view()), "GET /api/projects/p1/insights": reply(500, { error: { code: "internal", message: "boom" } }) });
    await renderApp("/p/p1?tab=insights");
    expect((await page().findAllByText(/boom/)).length).toBeGreaterThan(0);
  });

  it("is also a personal page across projects", async () => {
    const m = mockFetch({ ...shellRoutes(), "GET /api/me/insights": view({ projectId: undefined }) });
    await renderApp("/insights");
    expect(await page().findByText("My insights")).toBeTruthy();
    expect(m.calls.some((c) => c.url.startsWith("/api/me/insights?range=30d"))).toBe(true);
  });
});

describe("insights formatting", () => {
  it("formats durations and money", () => {
    expect(duration(0)).toBe("0s");
    expect(duration(45)).toBe("45s");
    expect(duration(11400)).toBe("3h 10m");
    expect(duration(2 * 86400 + 4 * 3600)).toBe("2d 4h");
    expect(money(41.2)).toBe("$41.20");
    expect(money(0.004)).toBe("$0.0040");
  });
});
