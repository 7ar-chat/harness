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
    "GET /api/projects/p1/board": { columns: [] },
  };
};

describe("ProjectPage", () => {
  it("renders the project with its sessions, repository and role", async () => {
    mockFetch(routes("owner"));
    await renderApp("/p/p1?tab=overview");
    expect(await screen.findByRole("heading", { name: /payments/ })).toBeTruthy();
    expect(screen.getByText("The payments service")).toBeTruthy();
    expect(screen.getByRole("link", { name: /github.com\/acme\/payments/ }).getAttribute("href")).toBe("https://github.com/acme/payments");
    expect(screen.getByText("main")).toBeTruthy();
    expect((await page().findByRole("link", { name: /Flaky test/ })).getAttribute("href")).toBe("/#/t/t1");
  });

  it("says there are no sessions yet for a project without any", async () => {
    mockFetch(routes("owner", []));
    await renderApp("/p/p1?tab=overview");
    expect(await screen.findByText("No sessions yet", { selector: "div" })).toBeTruthy();
  });

  it("opens on the board: sessions as cards in their columns, with the pull request of the ones in review", async () => {
    const card = (threadId: string, title: string, extra = {}) => ({ threadId, title, status: "idle", updatedAt: "2026-11-04T10:00:00Z", ...extra });
    mockFetch({
      ...routes("owner"),
      "GET /api/projects/p1/board": {
        columns: [
          { id: "ready", title: "Ready", cards: [card("t1", "Explain the retry logic")] },
          { id: "running", title: "Running", cards: [card("t2", "Add rate limiting", { status: "running" })] },
          { id: "needs_you", title: "Needs you", cards: [card("t3", "Rename the table", { status: "waiting_input", why: "Which name do you prefer?" })] },
          { id: "review", title: "In review", cards: [card("t4", "Fix login", { taskState: "review", prNumber: 12, prUrl: "https://github.com/acme/payments/pull/12" })] },
          { id: "done", title: "Done", cards: [] },
        ],
      },
    });
    await renderApp("/p/p1");
    const needs = await page().findByRole("region", { name: "Needs you" });
    expect(within(needs).getByRole("link", { name: /Rename the table/ }).getAttribute("href")).toBe("/#/t/t3");
    expect(within(needs).getByText("Which name do you prefer?")).toBeTruthy();
    const review = page().getByRole("region", { name: "In review" });
    expect(within(review).getByText("#12")).toBeTruthy();
    expect(within(page().getByRole("region", { name: "Running" })).getByRole("link", { name: /Add rate limiting/ })).toBeTruthy();
    expect(within(page().getByRole("region", { name: "Ready" })).getByRole("link", { name: /Explain the retry logic/ })).toBeTruthy();
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

  it("links several repositories to one project: shows them and saves the list from the settings", async () => {
    const p = project("p1", { name: "payments", repoUrl: "https://github.com/acme/payments", defaultBranch: "main", role: "owner", extraRepos: ["https://github.com/acme/web"] });
    const f = mockFetch({ ...routes("owner"), "GET /api/projects/p1": p, "PATCH /api/projects/p1": p });
    const { router } = await renderApp("/p/p1?tab=overview");
    expect(await screen.findByRole("link", { name: /github.com\/acme\/web/ })).toBeTruthy();
    await router.navigate({ to: "/p/$projectId", params: { projectId: "p1" }, search: { tab: "settings" } });
    const box = (await screen.findByLabelText("More repositories")) as HTMLTextAreaElement;
    expect(box.value).toBe("https://github.com/acme/web");
    fireEvent.change(box, { target: { value: "https://github.com/acme/web\nhttps://github.com/acme/docs" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(f.find("PATCH", "/api/projects/p1")).toHaveLength(1));
    expect((f.find("PATCH", "/api/projects/p1")[0]?.body as { extraRepos: string[] }).extraRepos).toEqual(["https://github.com/acme/web", "https://github.com/acme/docs"]);
  });
});
