import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { mockFetch, reply } from "@/test/fetch";
import { me, overview, project, shellRoutes } from "@/test/fixtures";
import { renderApp } from "@/test/render";
import { cacheHit, money, tokens } from "./Usage";

const totals = (over: object = {}) => ({ runs: 3, inputTokens: 30_000, outputTokens: 2_000, cacheReadTokens: 18_000, cacheWriteTokens: 3_000, costUsd: 0.4321, unpriced: 0, ...over });
const view = (over: object = {}) => ({
  scope: "me", from: "2026-11-01T00:00:00Z", to: "2026-11-23T00:00:00Z", group: "day", total: totals(),
  bySource: { session: totals({ runs: 2, costUsd: 0.3 }), schedule: totals({ runs: 1, costUsd: 0.1321 }) },
  rows: [{ key: "2026-11-20", ...totals({ runs: 1 }) }, { key: "2026-11-21", ...totals({ runs: 2, costUsd: 0.3 }) }],
  cap: { monthlyUsd: 0, spentUsd: 0.4321, ratio: 0, warn: false, reached: false }, canSetCap: true, ...over,
});
const page = () => within(screen.getByRole("main"));

describe("Usage page", () => {
  it("shows the cost, the cache split, the kinds of work and the daily chart", async () => {
    const m = mockFetch({ ...shellRoutes(), "GET /api/usage": view() });
    await renderApp("/usage");
    expect((await page().findAllByText("$0.43")).length).toBeGreaterThan(0);
    expect(page().getByText("60%")).toBeTruthy(); // 18k of 30k input read from the cache
    expect(page().getByLabelText(/Fresh input 28%, Cache read 56%, Cache write 9%, Output 6%/, { selector: "div" })).toBeTruthy();
    expect(page().getByText("Interactive sessions")).toBeTruthy();
    expect(page().getByText("Scheduled tasks")).toBeTruthy();
    expect(page().getByRole("img", { name: /Cost per day, 2 days/ })).toBeTruthy();
    expect(m.find("GET", "/api/usage?scope=me&range=month&group=day").length).toBeGreaterThan(0);
  });

  it("changes the grouping and shows the sessions by name with a link", async () => {
    const m = mockFetch({
      ...shellRoutes(),
      "GET /api/usage": (c: { url: string }) =>
        c.url.includes("group=thread")
          ? view({ group: "thread", rows: [{ key: "th1", ...totals() }], titles: { th1: "Fix the build" } })
          : c.url.includes("group=model")
            ? view({ group: "model", rows: [{ key: "gpt-a", ...totals({ unpriced: 1 }) }] })
            : view(),
    });
    await renderApp("/usage");
    fireEvent.mouseDown(await page().findByRole("tab", { name: "By model" }), { button: 0 });
    const models = within(await page().findByRole("table", { name: /by model/i }));
    expect(models.getByText("gpt-a")).toBeTruthy();
    expect(models.getByText("unpriced")).toBeTruthy();
    fireEvent.mouseDown(page().getByRole("tab", { name: "By session" }), { button: 0 });
    const link = await page().findByRole("link", { name: "Fix the build" });
    expect(link.getAttribute("href")).toContain("/t/th1");
    expect(m.find("GET", "/api/usage?scope=me&range=month&group=thread").length).toBeGreaterThan(0);
  });

  it("asks for a custom range as two dates", async () => {
    const m = mockFetch({ ...shellRoutes(), "GET /api/usage": view() });
    await renderApp("/usage");
    fireEvent.mouseDown(await page().findByRole("tab", { name: "Custom" }), { button: 0 });
    fireEvent.change(await page().findByLabelText("From"), { target: { value: "2026-11-01" } });
    fireEvent.change(page().getByLabelText("To"), { target: { value: "2026-11-07" } });
    await waitFor(() => expect(m.calls.some((c) => c.url.includes("range=2026-11-01..2026-11-07"))).toBe(true));
  });

  it("warns at 80% of the cap, and says new runs are refused once it is used up", async () => {
    const warn = view({ cap: { monthlyUsd: 10, spentUsd: 8.5, ratio: 0.85, warn: true, reached: false } });
    mockFetch({ ...shellRoutes(), "GET /api/usage": warn });
    const { unmount } = await renderApp("/usage");
    expect((await page().findByRole("status")).textContent).toContain("85% of the monthly cap");
    expect(page().getByRole("progressbar", { name: /monthly cap/ }).getAttribute("aria-valuenow")).toBe("85");
    unmount();
    mockFetch({ ...shellRoutes(), "GET /api/usage": view({ cap: { monthlyUsd: 10, spentUsd: 10.2, ratio: 1.02, warn: true, reached: true } }) });
    await renderApp("/usage");
    expect((await page().findByRole("alert")).textContent).toContain("new runs are refused");
  });

  it("saves the cap, and removes it with an empty field", async () => {
    const m = mockFetch({ ...shellRoutes(), "GET /api/usage": view(), "PUT /api/me/usage/cap": { monthlyUsd: 25, spentUsd: 0.43, ratio: 0.02, warn: false, reached: false } });
    await renderApp("/usage");
    fireEvent.change(await page().findByLabelText(/Cap \(USD per month/), { target: { value: "25" } });
    fireEvent.click(page().getByRole("button", { name: "Save cap" }));
    await waitFor(() => expect(m.find("PUT", "/api/me/usage/cap")).toHaveLength(1));
    expect(m.find("PUT", "/api/me/usage/cap")[0]?.body).toEqual({ monthlyUsd: 25 });
    fireEvent.change(page().getByLabelText(/Cap \(USD per month/), { target: { value: "-3" } });
    expect((page().getByRole("button", { name: "Save cap" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("shows the project's usage in its Usage tab, and only an owner can change the cap", async () => {
    const routes = (canSetCap: boolean) => ({
      ...shellRoutes(overview(), me({ projects: [project("p1", { role: canSetCap ? "owner" : "member" })] })),
      "GET /api/projects/p1": project("p1", { role: canSetCap ? "owner" : "member" }),
      "GET /api/usage": view({ scope: "project", projectId: "p1", canSetCap, cap: { monthlyUsd: 50, spentUsd: 5, ratio: 0.1, warn: false, reached: false } }),
      "PUT /api/projects/p1/usage/cap": { monthlyUsd: 60, spentUsd: 5, ratio: 0.08, warn: false, reached: false },
    });
    const m = mockFetch(routes(true));
    const first = await renderApp("/p/p1?tab=usage");
    fireEvent.change(await page().findByLabelText(/Cap \(USD per month/), { target: { value: "60" } });
    fireEvent.click(page().getByRole("button", { name: "Save cap" }));
    await waitFor(() => expect(m.find("PUT", "/api/projects/p1/usage/cap")).toHaveLength(1));
    expect(m.calls.some((c) => c.url.startsWith("/api/usage?scope=project&project=p1"))).toBe(true);
    first.unmount();
    mockFetch(routes(false));
    await renderApp("/p/p1?tab=usage");
    expect(await page().findByText(/of \$50/)).toBeTruthy();
    expect(page().queryByLabelText(/Cap \(USD per month/)).toBeNull();
  });

  it("says so when the server keeps no ledger", async () => {
    mockFetch({ ...shellRoutes(), "GET /api/usage": reply(404, { error: { code: "not_found", message: "no" } }) });
    await renderApp("/usage");
    expect(await page().findByText("This server does not keep a usage ledger.")).toBeTruthy();
  });
});

describe("usage formatting", () => {
  it("formats money, token counts and the cache hit rate", () => {
    expect(money(0)).toBe("$0");
    expect(money(0.00234)).toBe("$0.0023");
    expect(money(12.345)).toBe("$12.35");
    expect(money(1234.5)).toBe("$1,234.50");
    expect(tokens(950)).toBe("950");
    expect(tokens(25_000)).toBe("25.0k");
    expect(tokens(3_400_000)).toBe("3.40M");
    expect(cacheHit(totals())).toBeCloseTo(0.6);
    expect(cacheHit(totals({ inputTokens: 0 }))).toBe(0);
  });
});
