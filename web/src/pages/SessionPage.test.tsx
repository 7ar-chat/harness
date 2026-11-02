import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { Message, Thread } from "@/api/types";
import { mockFetch, reply, type Call } from "@/test/fetch";
import { me, overview, project, shellRoutes, thread as summary } from "@/test/fixtures";
import { memoryList, memoryStats } from "@/test/memory";
import { renderApp } from "@/test/render";

const T = "2026-01-01T00:00:00Z";
const msg = (id: string, role: Message["role"], content: string, extra: Partial<Message> = {}): Message => ({ id, role, content, createdAt: T, ...extra });
const full = (extra: Partial<Thread> = {}): Thread => ({ id: "t1", title: "Flaky test", pinned: false, status: "idle", createdBy: "alice", createdAt: T, updatedAt: T, activeRun: null, pendingApproval: null, pendingQuestion: null, messages: [], ...extra });
const routes = (t: unknown, extra: Record<string, unknown> = {}) => ({
  ...shellRoutes(overview({ personal: [summary("t1", { title: "Flaky test" })] })),
  "GET /api/threads/t1": t,
  "GET /api/models": { models: ["gpt-test"], default: "gpt-test" },
  "GET /api/commands": [],
  "GET /api/threads/t1/files": [],
  "GET /api/memory/stats": memoryStats(),
  "GET /api/memory": memoryList(),
  ...extra,
});
const page = () => within(screen.getByRole("main"));

describe("SessionPage", () => {
  it("shows the conversation of the thread with its title and status", async () => {
    // Arrange
    mockFetch(routes(full({ messages: [msg("u1", "user", "Why is the test flaky?", { author: { login: "alice" } }), msg("a1", "assistant", "It depends on the clock.")] })));
    // Act
    await renderApp("/t/t1");
    // Assert
    expect(await page().findByRole("heading", { name: "Flaky test" })).toBeTruthy();
    expect(await page().findByText("Why is the test flaky?")).toBeTruthy();
    expect(await page().findByText("It depends on the clock.")).toBeTruthy();
    expect(page().queryByText(/Send a message to start/)).toBeNull();
  });

  it("invites you to start when the thread has no messages", async () => {
    // Arrange
    mockFetch(routes(full()));
    // Act
    await renderApp("/t/t1");
    // Assert
    expect(await page().findByText(/Send a message to start/)).toBeTruthy();
  });

  it("shows the server error with a retry that refetches", async () => {
    // Arrange
    let fail = true;
    mockFetch(routes(null, { "GET /api/threads/t1": () => (fail ? reply(500, { error: { code: "internal", message: "database is down" } }) : full({ title: "Back again" })) }));
    await renderApp("/t/t1");
    expect(await screen.findByText("database is down", {}, { timeout: 4000 })).toBeTruthy();
    // Act
    fail = false;
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    // Assert
    expect(await page().findByRole("heading", { name: "Back again" })).toBeTruthy();
  });

  it("says the session is not found for a missing thread", async () => {
    // Arrange
    mockFetch(routes(null, { "GET /api/threads/t1": reply(404, { error: { code: "not_found", message: "no such thread" } }) }));
    // Act
    await renderApp("/t/t1");
    // Assert
    expect(await screen.findByText("Not found")).toBeTruthy();
  });

  it("sending a message POSTs a run with the page's settings and reloads the thread", async () => {
    // Arrange
    let threads = 0;
    const f = mockFetch(
      routes(null, {
        "GET /api/threads/t1": () => (++threads === 1 ? full() : full({ messages: [msg("u1", "user", "run the tests", { runId: "r1" }), msg("a1", "assistant", "All green.", { runId: "r1" })] })),
        "POST /api/threads/t1/runs": { id: "r1", threadId: "t1", status: "running" },
        "GET /api/runs/r1/events": (_c: Call) => reply(200, 'id: 1\ndata: {"type":"RUN_FINISHED","runId":"r1"}\n\n', { headers: { "Content-Type": "text/event-stream" } }),
      }),
    );
    await renderApp("/t/t1");
    const box = await page().findByRole("textbox", { name: "Message" });
    // Act
    fireEvent.change(box, { target: { value: "run the tests" } });
    fireEvent.click(page().getByRole("button", { name: /Send/ }));
    // Assert
    await waitFor(() => expect(f.find("POST", "/api/threads/t1/runs")).toHaveLength(1));
    expect(f.find("POST", "/api/threads/t1/runs")[0]?.body).toEqual({ message: "run the tests", model: "gpt-test", interactive: true, mode: "build" });
    expect(await page().findByText("All green.")).toBeTruthy();
  });

  it("a viewer sees the conversation but cannot send", async () => {
    // Arrange
    const p = project("p1", { name: "payments", role: "viewer" });
    const f = mockFetch({
      ...routes(full({ projectId: "p1", role: "viewer", messages: [msg("a1", "assistant", "Read me")] })),
      ...shellRoutes(overview({ projects: [{ ...p, threads: [summary("t1", { projectId: "p1" })] }] }), me({ projects: [p] })),
      "GET /api/projects/p1/members": [],
    });
    // Act
    await renderApp("/t/t1");
    // Assert
    expect(await page().findByText("Read me")).toBeTruthy();
    expect((await page().findByPlaceholderText("You have read-only access")) as HTMLTextAreaElement).toBeTruthy();
    expect(page().queryByRole("button", { name: "Rename session" })).toBeNull();
    expect(f.find("POST", "/api/threads/t1/runs")).toHaveLength(0);
  });
});
