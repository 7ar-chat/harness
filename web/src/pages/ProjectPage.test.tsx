import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { mockFetch, reply } from "@/test/fetch";
import { me, overview, project, shellRoutes, thread } from "@/test/fixtures";
import { renderApp } from "@/test/render";

const page = () => within(screen.getByRole("main"));

const routes = (role: "owner" | "member" | "viewer", threads = [thread("t1", { title: "Flaky test", status: "running" })]) => {
  const p = project("p1", { name: "payments", description: "The payments service", repoUrl: "https://github.com/acme/payments", defaultBranch: "main", role });
  return {
    ...shellRoutes(overview({ projects: [{ ...p, threads }] }), me({ projects: [p] })),
    "GET /api/projects/p1": p,
    "GET /api/projects/p1/members": [{ projectId: "p1", login: "alice", role, name: "Alice Doe", addedAt: "" }],
    "GET /api/projects/p1/github": reply(404),
    "GET /api/memory/stats": reply(404),
    "GET /api/memory": { entries: [], enabled: true },
  };
};

describe("ProjectPage", () => {
  it("renders the project with its sessions, repository and role", async () => {
    mockFetch(routes("owner"));
    await renderApp("/p/p1");
    expect(await screen.findByRole("heading", { name: /payments/ })).toBeTruthy();
    expect(screen.getByText("The payments service")).toBeTruthy();
    expect(screen.getByRole("link", { name: /github.com\/acme\/payments/ }).getAttribute("href")).toBe("https://github.com/acme/payments");
    expect(screen.getByText("main")).toBeTruthy();
    expect((await page().findByRole("link", { name: /Flaky test/ })).getAttribute("href")).toBe("/#/t/t1");
  });

  it("says there are no sessions yet for a project without any", async () => {
    mockFetch(routes("owner", []));
    await renderApp("/p/p1");
    expect(await screen.findByText("No sessions yet", { selector: "div" })).toBeTruthy();
  });

  it("gives owners a Settings tab and writers a New session button; viewers get neither", async () => {
    mockFetch(routes("owner"));
    const owner = await renderApp("/p/p1");
    await screen.findByRole("heading", { name: /payments/ });
    expect(screen.getByRole("tab", { name: "Settings" })).toBeTruthy();
    expect(page().getByRole("button", { name: /New session/ })).toBeTruthy();
    owner.unmount();

    mockFetch(routes("member"));
    const member = await renderApp("/p/p1");
    await screen.findByRole("heading", { name: /payments/ });
    expect(screen.queryByRole("tab", { name: "Settings" })).toBeNull();
    expect(page().getByRole("button", { name: /New session/ })).toBeTruthy();
    member.unmount();

    mockFetch(routes("viewer"));
    await renderApp("/p/p1");
    await screen.findByRole("heading", { name: /payments/ });
    expect(screen.queryByRole("tab", { name: "Settings" })).toBeNull();
    expect(page().queryByRole("button", { name: /New session/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /Import AGENTS.md/ })).toBeNull();
  });

  it("switching tab puts it in the URL", async () => {
    mockFetch(routes("owner"));
    const { router } = await renderApp("/p/p1");
    await screen.findByRole("heading", { name: /payments/ });
    fireEvent.mouseDown(screen.getByRole("tab", { name: "Members" }), { button: 0 });
    fireEvent.click(screen.getByRole("tab", { name: "Members" }));
    await waitFor(() => expect(router.state.location.search).toEqual({ tab: "members" }));
    expect(await screen.findByText("Alice Doe")).toBeTruthy();
  });

  it("shows 'Not found' without a retry button for a project that does not exist or is not yours", async () => {
    mockFetch({ ...routes("owner"), "GET /api/projects/p1": reply(404, { error: { code: "not_found", message: "nope" } }) });
    await renderApp("/p/p1");
    expect(await screen.findByText("Not found")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
  });

  it("shows an access message for 403 and a retry for a server error", async () => {
    mockFetch({ ...routes("owner"), "GET /api/projects/p1": reply(403, { error: { code: "forbidden", message: "no" } }) });
    const a = await renderApp("/p/p1");
    expect(await screen.findByText("You do not have access to this")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
    a.unmount();

    mockFetch({ ...routes("owner"), "GET /api/projects/p1": reply(500, { error: { code: "internal", message: "kaboom" } }) });
    await renderApp("/p/p1");
    const alert = await screen.findByText("kaboom");
    expect(within(alert.parentElement!).getByRole("button", { name: "Try again" })).toBeTruthy();
  });
});
