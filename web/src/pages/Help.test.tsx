import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { mockFetch, reply } from "@/test/fetch";
import { me, overview, project, shellRoutes } from "@/test/fixtures";
import { renderApp } from "@/test/render";

const page = () => within(screen.getByRole("main"));

describe("Help center", () => {
  it("lists the articles by group", async () => {
    mockFetch(shellRoutes());
    await renderApp("/help");
    expect(await page().findByRole("heading", { name: "Concepts" })).toBeTruthy();
    expect(page().getByText("Policies: supervised, sandboxed, autonomous")).toBeTruthy();
    expect(page().getByText("Keyboard shortcuts", { selector: "div" })).toBeTruthy();
  });

  it("finds an article by search and opens it", async () => {
    mockFetch(shellRoutes());
    await renderApp("/help");
    fireEvent.change(await page().findByRole("searchbox", { name: "Search the help" }), { target: { value: "autonomous" } });
    const results = within(page().getByRole("list", { name: "Search results" }));
    expect(results.getAllByRole("button").length).toBeGreaterThan(0);
    fireEvent.click(results.getByText("Policies: supervised, sandboxed, autonomous"));
    expect(await page().findByRole("heading", { name: "Policies: supervised, sandboxed, autonomous", level: 2 })).toBeTruthy();
    expect(page().getByText(/Every risky call asks you first/)).toBeTruthy();
  });

  it("says so when nothing matches", async () => {
    mockFetch(shellRoutes());
    await renderApp("/help");
    fireEvent.change(await page().findByRole("searchbox", { name: "Search the help" }), { target: { value: "qqzzxx" } });
    expect(page().getByRole("status").textContent).toMatch(/No article matches/);
  });

  it("opens the article of a deep link", async () => {
    mockFetch(shellRoutes());
    await renderApp("/help?a=budgets");
    expect(await page().findByRole("heading", { name: "Budgets and spend caps", level: 2 })).toBeTruthy();
    fireEvent.click(page().getByRole("button", { name: /All articles/ }));
    expect(await page().findByRole("searchbox", { name: "Search the help" })).toBeTruthy();
  });

  it("follows a link from one article to another", async () => {
    mockFetch(shellRoutes());
    await renderApp("/help?a=approvals");
    fireEvent.click(await page().findByRole("button", { name: "policy" }));
    expect(await page().findByRole("heading", { name: /^Policies/, level: 2 })).toBeTruthy();
  });
});

describe("contextual help", () => {
  const p = project("p1", { name: "payments" });
  const routes = () => ({
    ...shellRoutes(overview({ projects: [{ ...p, threads: [] }] }), me({ projects: [p] })),
    "GET /api/projects/p1": p,
    "GET /api/memory/stats": reply(404),
  });

  it("a question mark beside a section opens its article in a side sheet", async () => {
    mockFetch(routes());
    await renderApp("/p/p1?tab=schedules");
    fireEvent.click(await page().findByRole("button", { name: /\(help\)$/ }));
    const sheet = await screen.findByRole("dialog");
    expect(within(sheet).getByRole("heading", { name: "Schedule recurring work" })).toBeTruthy();
    expect(within(sheet).getByRole("button", { name: "Open in the help center" })).toBeTruthy();
    fireEvent.keyDown(sheet, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });
});
