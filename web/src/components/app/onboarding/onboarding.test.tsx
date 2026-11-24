import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it } from "vitest";
import { mockFetch, reply } from "@/test/fetch";
import { me, overview, project, shellRoutes, thread } from "@/test/fixtures";
import { renderApp } from "@/test/render";

const main = () => within(screen.getByRole("main"));
const card = () => within(main().getByRole("region", { name: "Getting started" }));
const stored = () => JSON.parse(localStorage.getItem("7ar.onboarding.v1.alice") ?? "{}") as Record<string, unknown>;

beforeEach(() => localStorage.setItem("7ar.onboarding.force", "1"));

const withProject = () => {
  const p = project("p1", { name: "payments", repoUrl: "https://github.com/acme/payments" });
  return { p, routes: { ...shellRoutes(overview({ projects: [{ ...p, threads: [thread("t1")] }] }), me({ projects: [p] })), "GET /api/projects/p1": p, "GET /api/memory/stats": reply(404) } };
};

describe("first-run welcome", () => {
  it("opens for a new person, can be skipped, stays away, and reopens from the user menu", async () => {
    mockFetch(shellRoutes());
    await renderApp("/");
    const dlg = await screen.findByRole("dialog", { name: "Welcome to 7ar" });
    fireEvent.click(within(dlg).getByRole("button", { name: "Skip for now" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(stored().welcomed).toBe(true);
    fireEvent.pointerDown(screen.getByRole("button", { name: /^User menu/ }), { button: 0, ctrlKey: false });
    fireEvent.click(await screen.findByRole("menuitem", { name: /Getting started/ }));
    expect(await screen.findByRole("dialog", { name: "Welcome to 7ar" })).toBeTruthy();
  });

  it("does not open again for someone who has been welcomed", async () => {
    localStorage.setItem("7ar.onboarding.v1.alice", JSON.stringify({ welcomed: true }));
    mockFetch(shellRoutes());
    await renderApp("/");
    await main().findByText("Create your first project");
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});

describe("getting-started checklist", () => {
  beforeEach(() => localStorage.setItem("7ar.onboarding.v1.alice", JSON.stringify({ welcomed: true })));

  it("starts at zero for a newcomer and the project step opens the project dialog", async () => {
    mockFetch(shellRoutes());
    await renderApp("/");
    expect(await card().findByText(/0 of 7 done/)).toBeTruthy();
    fireEvent.click(card().getByRole("button", { name: /Create a project/ }));
    expect(await screen.findByRole("dialog", { name: "New project" })).toBeTruthy();
  });

  it("ticks steps from real data: a project, a session, a repository", async () => {
    mockFetch(withProject().routes);
    await renderApp("/");
    expect(await card().findByText(/3 of 7 done/)).toBeTruthy();
    const bar = card().getByRole("progressbar", { name: "Getting started progress" });
    expect(bar.getAttribute("aria-valuenow")).toBe("3");
    expect(card().getByRole("button", { name: /Create a project.*\(done\)/ })).toBeTruthy();
  });

  it("ticks tasks, teammates and the spend cap from their own data", async () => {
    const { routes } = withProject();
    mockFetch({
      ...routes,
      "GET /api/projects/p1/tasks?limit=500": [{ id: "a", title: "x" }],
      "GET /api/projects/p1/members": [{ login: "alice", role: "owner" }, { login: "bob", role: "member" }],
      "GET /api/usage?scope=project&project=p1&range=month&group=day": { total: { costUsd: 1 }, rows: [], cap: { monthlyUsd: 50, spentUsd: 1, ratio: 0.02 } },
    });
    await renderApp("/");
    expect(await card().findByText(/6 of 7 done/)).toBeTruthy();
  });

  it("an approval (marked from anywhere) ticks its step, and the links go where they say", async () => {
    localStorage.setItem("7ar.onboarding.v1.alice", JSON.stringify({ welcomed: true, marked: { approved: true } }));
    mockFetch(withProject().routes);
    await renderApp("/");
    expect(await card().findByText(/4 of 7 done/)).toBeTruthy();
    expect(card().getByRole("link", { name: /Invite a teammate/ }).getAttribute("href")).toContain("tab=members");
    expect(card().getByRole("link", { name: /Set a spend cap/ }).getAttribute("href")).toContain("tab=usage");
  });

  it("ticks the approval step from the server's audit counts, in a browser that never approved", async () => {
    mockFetch({ ...withProject().routes, "GET /api/me/activity-summary": { approvalsDecided: 2, tasksHandedOff: 0, sessionsStarted: 1, membersInvited: 0 } });
    await renderApp("/");
    expect(await card().findByText(/4 of 7 done/)).toBeTruthy();
    expect(card().getByRole("link", { name: /Approve your first action.*\(done\)/ })).toBeTruthy();
  });

  it("leaves the approval step open when the server counts none and nothing was marked", async () => {
    mockFetch({ ...withProject().routes, "GET /api/me/activity-summary": { approvalsDecided: 0, tasksHandedOff: 0, sessionsStarted: 1, membersInvited: 0 } });
    await renderApp("/");
    expect(await card().findByText(/3 of 7 done/)).toBeTruthy();
    expect(card().getByRole("link", { name: /Approve your first action.*\(to do\)/ })).toBeTruthy();
  });

  it("can be dismissed for good", async () => {
    mockFetch(shellRoutes());
    await renderApp("/");
    fireEvent.click(await card().findByRole("button", { name: "Dismiss" }));
    await waitFor(() => expect(main().queryByRole("region", { name: "Getting started" })).toBeNull());
    expect(stored().dismissed).toBe(true);
  });

  it("is a folded progress line in the explorer, opened on demand", async () => {
    mockFetch(shellRoutes());
    await renderApp("/");
    const side = within(await screen.findByRole("navigation", { name: "Explorer" }));
    const toggle = await side.findByRole("button", { name: /Getting started/ });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    expect(stored().folded).toBe(false);
  });
});

describe("try it with a sample", () => {
  it("creates a project from a template, a session in it and starts the first run", async () => {
    localStorage.setItem("7ar.onboarding.v1.alice", JSON.stringify({ welcomed: true }));
    const m = mockFetch({
      ...shellRoutes(),
      "GET /api/templates": [{ id: "react-vite", name: "React", description: "" }, { id: "static-html", name: "Static", description: "" }],
      "POST /api/projects": { id: "np", name: "Sample project" },
      "POST /api/threads": { id: "nt", title: "Try 7ar" },
      "POST /api/threads/nt/runs": { id: "r1" },
    });
    await renderApp("/");
    fireEvent.click(await main().findByRole("button", { name: /Try it with a sample/ }));
    await waitFor(() => expect(m.find("POST", "/api/threads/nt/runs")).toHaveLength(1));
    expect(m.find("POST", "/api/projects")[0]?.body).toMatchObject({ template: "static-html" });
    expect(m.find("POST", "/api/threads")[0]?.body).toMatchObject({ projectId: "np" });
    expect((m.find("POST", "/api/threads/nt/runs")[0]?.body as { message: string }).message.length).toBeGreaterThan(20);
  });
});

describe("discoverability", () => {
  beforeEach(() => localStorage.setItem("7ar.onboarding.v1.alice", JSON.stringify({ welcomed: true, dismissed: true })));

  it("? opens the shortcuts sheet", async () => {
    mockFetch(shellRoutes());
    await renderApp("/");
    fireEvent.keyDown(document.body, { key: "?" });
    const sheet = await screen.findByRole("dialog", { name: "Keyboard shortcuts" });
    expect(within(sheet).getByText("Command palette: jump anywhere, run any action")).toBeTruthy();
  });

  it("the palette suggests things to try and offers help commands", async () => {
    mockFetch(shellRoutes());
    await renderApp("/");
    fireEvent.keyDown(window, { key: "k", ctrlKey: true });
    const list = within(await screen.findByRole("listbox"));
    expect(list.getByText("Try: start a session and ask an agent")).toBeTruthy();
    expect(list.getByText(/Show keyboard shortcuts/)).toBeTruthy();
    expect(list.getByText("Open help")).toBeTruthy();
    fireEvent.click(list.getByText(/Show keyboard shortcuts/));
    expect(await screen.findByRole("dialog", { name: "Keyboard shortcuts" })).toBeTruthy();
  });

  it("the tour runs from the user menu and closes with Escape", async () => {
    mockFetch(shellRoutes());
    await renderApp("/");
    fireEvent.pointerDown(screen.getByRole("button", { name: /^User menu/ }), { button: 0, ctrlKey: false });
    fireEvent.click(await screen.findByRole("menuitem", { name: /Take the tour/ }));
    const tour = await screen.findByTestId("tour");
    expect(within(tour).getByRole("heading", { name: "The explorer" })).toBeTruthy();
    fireEvent.click(within(tour).getByRole("button", { name: "Next" }));
    expect(within(tour).getByRole("heading", { name: "Editor tabs" })).toBeTruthy();
    fireEvent.keyDown(window, { key: "Escape" });
    await waitFor(() => expect(screen.queryByTestId("tour")).toBeNull());
    expect(stored().toured).toBe(true);
  });
});
