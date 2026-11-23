import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { describe, expect, it, vi } from "vitest";
import { mockFetch, reply } from "@/test/fetch";
import { me, overview, project, shellRoutes, thread } from "@/test/fixtures";
import { renderApp } from "@/test/render";

const iso = (minAgo: number) => new Date(Date.now() - minAgo * 60_000).toISOString();

describe("Dashboard", () => {
  it("invites you to create a project when you have nothing yet", async () => {
    // Arrange
    mockFetch(shellRoutes(overview()));
    // Act
    await renderApp("/");
    // Assert
    expect(await screen.findByText("Create your first project")).toBeTruthy();
    expect(screen.getByText("Nothing is running. Start a session from the sidebar.")).toBeTruthy();
    expect(screen.queryByText("Recent sessions")).toBeNull();
  });

  it("shows the server error with a retry that refetches", async () => {
    // Arrange
    let fail = true;
    mockFetch({ ...shellRoutes(), "GET /api/overview": () => (fail ? reply(500, { error: { code: "internal", message: "database is down" } }) : overview({ personal: [thread("t1", { title: "Back again" })] })) });
    await renderApp("/");
    expect(await screen.findByText("Something went wrong")).toBeTruthy();
    expect(screen.getByText("database is down")).toBeTruthy();
    // Act
    fail = false;
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    // Assert
    expect(await screen.findByText("Recent sessions")).toBeTruthy();
    expect(screen.getAllByText("Back again").length).toBeGreaterThan(0);
  });

  it("puts sessions that need you first, then recent idle ones, and hides the assistant thread", async () => {
    // Arrange
    const o = overview({
      personal: [thread("sv", { title: "Global agent", kind: "supervisor", status: "waiting_input" }), thread("p1", { title: "Scratch notes", updatedAt: iso(300) })],
      projects: [
        {
          ...project("pr", { name: "payments" }),
          threads: [
            thread("t-run", { title: "Running one", status: "running", updatedAt: iso(1) }),
            thread("t-fail", { title: "Failed one", status: "failed", updatedAt: iso(50) }),
            thread("t-appr", { title: "Approve one", status: "waiting_approval", pendingTool: "bash", updatedAt: iso(100) }),
            thread("t-ask", { title: "Question one", status: "waiting_input", pendingQuestion: "Which Go version?", updatedAt: iso(10) }),
            thread("t-idle", { title: "Old idle", updatedAt: iso(500) }),
          ],
        },
      ],
    });
    mockFetch(shellRoutes(o));
    // Act
    await renderApp("/");
    await screen.findByText("Running and waiting (4)");
    // Assert: the active section lists approval, question, failure, running in that order
    const active = screen.getByRole("region", { name: /Running and waiting/ });
    const links = within(active).getAllByRole("link").map((a) => a.textContent).filter((t) => t !== "Review");
    expect(links).toEqual(["Approve one", "Question one", "Failed one", "Running one"]);
    expect(within(active).getByText("bash")).toBeTruthy();
    expect(within(active).getByText("Which Go version?")).toBeTruthy();
    const recent = screen.getByRole("region", { name: "Recent sessions" });
    expect(within(recent).getAllByRole("link").map((a) => a.textContent)).toEqual(["Scratch notes", "Old idle"]);
    expect(screen.queryByText("Global agent", { selector: "a" })).toBeNull();
  });

  it("approving from a card fetches the pending approval, resumes the run and refreshes the overview", async () => {
    // Arrange
    const o = overview({ projects: [{ ...project("pr"), threads: [thread("t1", { title: "Needs ok", status: "waiting_approval", pendingTool: "bash" })] }] });
    const f = mockFetch({
      ...shellRoutes(o),
      "GET /api/threads/t1": { id: "t1", title: "Needs ok", messages: [], activeRun: null, pendingApproval: { id: "a1", toolName: "bash" }, pendingQuestion: null, status: "waiting_approval", createdAt: "", updatedAt: "" },
      "POST /api/threads/t1/resume": { id: "r2", threadId: "t1", status: "running" },
    });
    await renderApp("/");
    // Act
    fireEvent.click(await screen.findByRole("button", { name: /Approve/ }));
    // Assert
    await waitFor(() => expect(f.find("POST", "/api/threads/t1/resume")).toHaveLength(1));
    expect(f.find("POST", "/api/threads/t1/resume")[0]?.body).toEqual({ interruptId: "a1", reason: "tool_approval", decision: "approve", interactive: true });
    await waitFor(() => expect(f.find("GET", "/api/overview").length).toBeGreaterThan(1));
  });

  it("says so, and does not resume anything, when the approval is already gone", async () => {
    const o = overview({ projects: [{ ...project("pr"), threads: [thread("t1", { status: "waiting_approval" })] }] });
    const f = mockFetch({
      ...shellRoutes(o),
      "GET /api/threads/t1": { id: "t1", title: "x", messages: [], activeRun: null, pendingApproval: null, pendingQuestion: null, status: "idle", createdAt: "", updatedAt: "" },
    });
    const err = vi.spyOn(toast, "error").mockImplementation(() => "");
    await renderApp("/");
    fireEvent.click(await screen.findByRole("button", { name: /Deny/ }));
    await waitFor(() => expect(err).toHaveBeenCalledWith("Nothing to approve any more."));
    expect(f.find("POST", "/api/threads/t1/resume")).toHaveLength(0);
  });
});

describe("app shell", () => {
  it("shows the selected project's sessions and what needs you in the explorer, and the personal space apart", async () => {
    const o = overview({
      personal: [thread("p1", { title: "Scratch notes" })],
      projects: [{ ...project("pr", { name: "payments" }), threads: [thread("t1", { title: "Flaky test", status: "waiting_approval" }), thread("t2", { title: "Done", status: "idle" })] }],
    });
    const waiting = thread("t1", { title: "Flaky test", status: "waiting_approval" });
    mockFetch({
      ...shellRoutes(o, me({ projects: [project("pr", { name: "payments" })] })),
      "GET /api/projects/pr/members": [],
      "GET /api/needs-review": { tasks: [], gates: [], threads: [{ projectId: "pr", projectName: "payments", thread: waiting }], count: 1 },
    });
    await renderApp("/");
    const nav = within(await screen.findByRole("navigation", { name: "Explorer" }));
    expect((await screen.findByRole("button", { name: "Switch project" })).textContent).toContain("payments");
    const needs = within(nav.getByRole("region", { name: "Needs you" }));
    expect((await needs.findByRole("link", { name: /Flaky test/ })).getAttribute("href")).toBe("/#/t/t1");
    expect(nav.getByRole("region", { name: "Sessions" })).toBeTruthy();
    expect(nav.getByRole("link", { name: /Scratch notes/ }).getAttribute("href")).toBe("/#/t/p1");
    expect(nav.getByRole("link", { name: "Workflows & runs" }).getAttribute("href")).toBe("/#/p/pr?tab=workflows");
    expect(nav.getByLabelText("1 waiting for you")).toBeTruthy(); // the inbox count
  });

  it("offers read-only members no 'new session' action", async () => {
    const o = overview({ projects: [{ ...project("pr", { name: "ro", role: "viewer" }), threads: [] }, { ...project("pw", { name: "rw", role: "member" }), threads: [] }] });
    mockFetch(shellRoutes(o));
    const a = await renderApp("/p/pw");
    expect(await screen.findByLabelText("New session in rw")).toBeTruthy();
    a.unmount();
    await renderApp("/p/pr");
    await screen.findByRole("button", { name: "Switch project" });
    await waitFor(() => expect(screen.getByRole("button", { name: "Switch project" }).textContent).toContain("ro"));
    expect(screen.queryByLabelText("New session in ro")).toBeNull();
  });

  it("opens each place it is sent to as a tab, focuses a known one, and closes tabs", async () => {
    const o = overview({ projects: [{ ...project("pr", { name: "payments" }), threads: [thread("t1", { title: "Flaky test" })] }] });
    mockFetch({ ...shellRoutes(o), "GET /api/projects/pr": project("pr", { name: "payments" }), "GET /api/threads/t1": { ...thread("t1"), messages: [], activeRun: null, pendingApproval: null, pendingQuestion: null } });
    const { router } = await renderApp("/");
    const strip = within(await screen.findByRole("tablist", { name: "Open editors" }));
    expect(strip.getByRole("tab", { name: "Dashboard", selected: true })).toBeTruthy();
    fireEvent.click(screen.getByRole("link", { name: "Members & access" }));
    await waitFor(() => expect(strip.getByRole("tab", { name: "Members & access", selected: true })).toBeTruthy());
    expect(strip.getAllByRole("tab")).toHaveLength(2);
    fireEvent.click(screen.getByRole("link", { name: "Members & access" })); // focusing it again adds nothing
    expect(strip.getAllByRole("tab")).toHaveLength(2);
    fireEvent.click(strip.getByRole("tab", { name: "Dashboard" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/"));
    fireEvent.click(strip.getByRole("button", { name: "Close Members & access" }));
    await waitFor(() => expect(strip.getAllByRole("tab")).toHaveLength(1));
  });
});
