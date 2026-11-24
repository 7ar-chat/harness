import { screen, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { overview, project, shellRoutes } from "@/test/fixtures";
import { mockFetch, reply } from "@/test/fetch";
import { renderApp } from "@/test/render";
import { tabFor } from "@/components/app/shell/tabs";

describe("router", () => {
  it("shows a not-found page with a way back to the dashboard for an unknown route", async () => {
    // Arrange
    mockFetch({ ...shellRoutes() });
    // Act
    const { router } = await renderApp("/no/such/page");
    // Assert
    expect(await screen.findByText("Page not found")).toBeTruthy();
    const back = screen.getByRole("link", { name: "Back to the dashboard" });
    expect(back.getAttribute("href")).toBe("/#/");
    expect(router.state.location.pathname).toBe("/no/such/page");
  });

  it("keeps only a known tab in the project search", async () => {
    mockFetch({ ...shellRoutes(overview({ projects: [{ ...project("p1"), threads: [] }] })), "GET /api/projects/p1": project("p1") });
    const bogus = await renderApp("/p/p1?tab=bogus");
    await waitFor(() => expect(bogus.router.state.status).toBe("idle"));
    expect(bogus.router.state.location.search).toEqual({ tab: undefined });
    bogus.unmount();
    const ok = await renderApp("/p/p1?tab=members");
    await waitFor(() => expect(ok.router.state.status).toBe("idle"));
    expect(ok.router.state.location.search).toEqual({ tab: "members" });
  });

  it("keeps the requirement, task, workflow and run of a project place in the search, each its own editor tab", async () => {
    mockFetch({ ...shellRoutes(overview({ projects: [{ ...project("p1"), threads: [] }] })), "GET /api/projects/p1": project("p1") });
    const r = await renderApp("/p/p1?tab=requirements&req=r1");
    await waitFor(() => expect(r.router.state.status).toBe("idle"));
    expect(r.router.state.location.search).toEqual({ tab: "requirements", req: "r1" });
    expect(tabFor("/p/p1", { tab: "requirements", req: "r1" })?.key).toBe("p:p1:req:r1");
    expect(tabFor("/p/p1", { tab: "requirements" })?.key).toBe("p:p1:work");
    expect(tabFor("/p/p1", { tab: "tasks", task: "t1" })?.kind).toBe("task");
    expect(tabFor("/p/p1", { tab: "workflows", run: "x" })?.kind).toBe("run");
    expect(tabFor("/p/p1", { tab: "workflows", wf: "w" })?.kind).toBe("workflow");
  });

  it("routes /t/$threadId with its id as a param", async () => {
    mockFetch({ ...shellRoutes(), "GET /api/threads/abc": reply(404, { error: { code: "not_found", message: "no" } }) });
    const { router } = await renderApp("/t/abc");
    await waitFor(() => expect(router.state.matches.at(-1)?.params).toEqual({ threadId: "abc" }));
    expect(await screen.findByText("Not found")).toBeTruthy();
  });
});
