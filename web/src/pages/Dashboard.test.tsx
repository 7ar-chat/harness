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
  it("lists my projects and personal sessions in the sidebar and marks needs-attention counts", async () => {
    const o = overview({
      personal: [thread("p1", { title: "Scratch notes" })],
      projects: [{ ...project("pr", { name: "payments" }), threads: [thread("t1", { title: "Flaky test", status: "failed" }), thread("t2", { title: "Done", status: "idle" })] }],
    });
    mockFetch({ ...shellRoutes(o, me({ projects: [project("pr", { name: "payments" })] })), "GET /api/projects/pr/members": [] });
    await renderApp("/");
    const nav = await screen.findByRole("link", { name: /payments/ });
    expect(nav.getAttribute("href")).toBe("/#/p/pr");
    expect(within(nav).getByText("1")).toBeTruthy(); // one session needs a person
    expect(screen.getAllByRole("link", { name: /Scratch notes/ }).length).toBeGreaterThan(0);
  });

  it("offers read-only members no 'new session' action", async () => {
    const o = overview({ projects: [{ ...project("pr", { name: "ro", role: "viewer" }), threads: [] }, { ...project("pw", { name: "rw", role: "member" }), threads: [] }] });
    mockFetch(shellRoutes(o));
    await renderApp("/");
    await screen.findByRole("link", { name: /rw/ });
    expect(screen.getByLabelText("New session in rw")).toBeTruthy();
    expect(screen.queryByLabelText("New session in ro")).toBeNull();
  });
});
