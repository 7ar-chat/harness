import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { FactoryTask, InboxDoc } from "@/api/types";
import { mockFetch, reply } from "@/test/fetch";
import { shellRoutes, thread } from "@/test/fixtures";
import { renderApp } from "@/test/render";

const page = () => within(screen.getByRole("main"));
const T = "2026-11-05T10:00:00Z";
const task = (id: string, extra: Partial<FactoryTask> = {}): FactoryTask => ({
  id, projectId: "p1", owner: "alice", title: `Task ${id}`, spec: "x", source: { kind: "manual" }, state: "blocked", attempts: 1, spent: { usd: 0, tokens: 0, runs: 0 }, createdAt: T, updatedAt: T, ...extra,
});
const doc = (extra: Partial<InboxDoc> = {}): InboxDoc => ({ tasks: [], threads: [], gates: [], count: 0, ...extra });

describe("Inbox", () => {
  it("lists blocked tasks, pull requests and waiting sessions", async () => {
    const blocked = task("a", { threadId: "th1" });
    const review = task("b", { state: "review", pr: { number: 9, url: "https://github.com/x/y/pull/9" } });
    mockFetch({
      ...shellRoutes(),
      "GET /api/needs-review": doc({
        count: 3,
        tasks: [
          { kind: "blocked", projectId: "p1", projectName: "payments", role: "owner", task: blocked, reason: "needs your call", lastEvent: { id: 1, taskId: "a", kind: "escalated", text: "escalated", at: T } },
          { kind: "review", projectId: "p1", projectName: "payments", role: "owner", task: review },
        ],
        threads: [{ projectId: "p1", projectName: "payments", thread: thread("t9", { title: "Ask me", status: "waiting_input", pendingQuestion: "Which database?" }) }],
      }),
    });
    await renderApp("/inbox");
    expect(await page().findByText("needs your call")).toBeTruthy();
    expect(page().getByRole("link", { name: /Review PR #9/ }).getAttribute("href")).toBe("https://github.com/x/y/pull/9");
    expect(page().getByText("Which database?")).toBeTruthy();
    expect(page().getByRole("link", { name: "Task a" }).getAttribute("href")).toBe("/#/p/p1?tab=tasks&task=a");
    expect(page().getByRole("link", { name: "Session" }).getAttribute("href")).toBe("/#/t/th1");
  });

  it("resumes a blocked task inline with feedback", async () => {
    const m = mockFetch({
      ...shellRoutes(),
      "GET /api/needs-review": doc({ count: 1, tasks: [{ kind: "blocked", projectId: "p1", projectName: "payments", role: "owner", task: task("a"), reason: "stuck" }] }),
      "POST /api/projects/p1/tasks/a/resume": reply(202),
    });
    await renderApp("/inbox");
    fireEvent.click(await page().findByRole("button", { name: "Resume" }));
    fireEvent.change(await page().findByLabelText("Feedback"), { target: { value: "go on" } });
    fireEvent.click(page().getAllByRole("button", { name: "Resume" }).at(-1)!);
    await waitFor(() => expect(m.find("POST", "/api/projects/p1/tasks/a/resume")).toHaveLength(1));
    expect(m.find("POST", "/api/projects/p1/tasks/a/resume")[0]?.body).toEqual({ feedback: "go on" });
  });

  it("says nothing waits when the inbox is empty", async () => {
    mockFetch({ ...shellRoutes(), "GET /api/needs-review": doc() });
    await renderApp("/inbox");
    expect(await page().findByText("Nothing waits for you")).toBeTruthy();
  });

  it("shows the count in the sidebar and on the dashboard", async () => {
    const it = (id: string) => ({ kind: "blocked" as const, projectId: "p1", projectName: "payments", role: "owner", task: task(id) });
    mockFetch({ ...shellRoutes(), "GET /api/needs-review": doc({ count: 2, tasks: [it("a"), it("b")] }) });
    await renderApp("/");
    expect((await screen.findAllByLabelText("2 waiting for you")).length).toBeGreaterThan(0); // the explorer and the status bar
    expect((await page().findByRole("link", { name: /2 items need your review/ })).getAttribute("href")).toBe("/#/inbox");
  });
  it("shows the workflow gates that wait for the caller and decides them inline", async () => {
    const m = mockFetch({
      ...shellRoutes(),
      "GET /api/needs-review": doc({
        count: 1,
        gates: [{ projectId: "p1", projectName: "payments", runId: "run1", workflowName: "Delivery", nodeId: "review", title: "Review the plan", approvers: ["role:member"], since: T, summary: "the plan so far" }],
      }),
      "POST /api/workflow-runs/run1/nodes/review/approve": { id: "run1" },
      "POST /api/workflow-runs/run1/nodes/review/send-back": { id: "run1" },
    });
    await renderApp("/inbox");
    expect(await page().findByText("the plan so far")).toBeTruthy();
    expect(page().getByRole("link", { name: "Delivery: Review the plan" }).getAttribute("href")).toBe("/#/p/p1?tab=workflows&run=run1");

    fireEvent.change(page().getByLabelText("Approval comment"), { target: { value: "ok" } });
    fireEvent.click(page().getByRole("button", { name: "Approve" }));
    await waitFor(() => expect(m.find("POST", "/api/workflow-runs/run1/nodes/review/approve")).toHaveLength(1));
    expect(m.find("POST", "/api/workflow-runs/run1/nodes/review/approve")[0]?.body).toEqual({ comment: "ok" });

    fireEvent.click(page().getByRole("button", { name: "Send back" }));
    fireEvent.change(page().getByLabelText(/What must change/), { target: { value: "more detail" } });
    fireEvent.click(page().getByRole("button", { name: "Send it back" }));
    await waitFor(() => expect(m.find("POST", "/api/workflow-runs/run1/nodes/review/send-back")).toHaveLength(1));
    expect(m.find("POST", "/api/workflow-runs/run1/nodes/review/send-back")[0]?.body).toEqual({ feedback: "more detail" });
  });
});
