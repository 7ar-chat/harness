import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { FactoryTask } from "@/api/types";
import { mockFetch, reply } from "@/test/fetch";
import { me, overview, project, shellRoutes } from "@/test/fixtures";
import { renderApp } from "@/test/render";

const page = () => within(screen.getByRole("main"));
const T = "2026-11-05T10:00:00Z";

const task = (id: string, extra: Partial<FactoryTask> = {}): FactoryTask => ({
  id, projectId: "p1", owner: "alice", title: `Task ${id}`, spec: "do the thing", source: { kind: "manual" }, state: "ready", attempts: 0, spent: { usd: 0, tokens: 0, runs: 0 }, createdAt: T, updatedAt: T, ...extra,
});

const routes = (role: "owner" | "member" | "viewer" = "owner", tasks: FactoryTask[] = []) => {
  const p = project("p1", { name: "payments", role });
  return {
    ...shellRoutes(overview({ projects: [{ ...p, threads: [] }] }), me({ projects: [p] })),
    "GET /api/projects/p1": p,
    "GET /api/memory/stats": reply(404),
    "GET /api/projects/p1/tasks?limit=500": tasks,
  };
};

describe("Tasks tab", () => {
  it("lists the tasks and filters them by state", async () => {
    mockFetch(routes("owner", [task("a", { state: "blocked", blockedReason: "needs a call" }), task("b", { state: "done" })]));
    await renderApp("/p/p1?tab=tasks");
    expect(await page().findByText("Task a")).toBeTruthy();
    expect(page().getByText("Task b")).toBeTruthy();
    fireEvent.click(within(page().getByRole("group", { name: "Filter by state" })).getByRole("button", { name: /^Blocked/ }));
    expect(page().queryByText("Task b")).toBeNull();
    expect(page().getByText("Task a")).toBeTruthy();
  });

  it("opens a task: state, attempts, spend against the budget, pull request, CI and the history", async () => {
    const t = task("a", { state: "review", attempts: 2, maxAttempts: 3, spent: { usd: 1.5, tokens: 2000, runs: 2 }, budget: { maxUsd: 5 }, pr: { number: 42, url: "https://github.com/x/y/pull/42" }, threadId: "th1" });
    mockFetch({
      ...routes("owner", [t]),
      "GET /api/projects/p1/tasks/a": {
        task: t,
        events: [
          { id: 1, taskId: "a", kind: "created", text: "created by alice", at: T },
          { id: 2, taskId: "a", kind: "ci", text: "checks passed", data: { state: "passed" }, at: T },
          { id: 3, taskId: "a", kind: "review", text: "approved", data: { verdict: "approve" }, at: T },
        ],
      },
    });
    await renderApp("/p/p1?tab=tasks&task=a");
    const d = within(await screen.findByRole("region", { name: "Task detail" }));
    expect(await d.findByText("2 of 3")).toBeTruthy();
    expect(d.getByText(/\$1\.50 of \$5\.00/)).toBeTruthy();
    expect(d.getByRole("link", { name: /#42/ }).getAttribute("href")).toBe("https://github.com/x/y/pull/42");
    expect(d.getByText("passed")).toBeTruthy();
    expect(d.getByText("approve")).toBeTruthy();
    expect(d.getByText("created by alice")).toBeTruthy();
    expect(d.getByRole("link", { name: /agent's session/ }).getAttribute("href")).toBe("/#/t/th1");
  });

  it("resumes a blocked task with feedback and a higher budget", async () => {
    const t = task("a", { state: "blocked", blockedReason: "budget reached", budget: { maxUsd: 2 } });
    const m = mockFetch({ ...routes("owner", [t]), "GET /api/projects/p1/tasks/a": { task: t, events: [] }, "POST /api/projects/p1/tasks/a/resume": reply(202) });
    await renderApp("/p/p1?tab=tasks&task=a");
    const d = within(await screen.findByRole("region", { name: "Task detail" }));
    fireEvent.change(await d.findByLabelText("Feedback"), { target: { value: "try the other API" } });
    fireEvent.change(d.getByLabelText("Budget in USD"), { target: { value: "6" } });
    fireEvent.click(d.getByRole("button", { name: "Resume" }));
    await waitFor(() => expect(m.find("POST", "/api/projects/p1/tasks/a/resume")).toHaveLength(1));
    expect(m.find("POST", "/api/projects/p1/tasks/a/resume")[0]?.body).toEqual({ feedback: "try the other API", budget: { maxUsd: 6 } });
  });

  it("cancels an active task after confirming", async () => {
    const t = task("a", { state: "running" });
    const m = mockFetch({ ...routes("member", [t]), "GET /api/projects/p1/tasks/a": { task: t, events: [] }, "POST /api/projects/p1/tasks/a/cancel": reply(202) });
    await renderApp("/p/p1?tab=tasks&task=a");
    fireEvent.click(await screen.findByRole("button", { name: "Cancel task" }));
    const confirm = await screen.findByRole("alertdialog");
    fireEvent.click(within(confirm).getByRole("button", { name: "Cancel task" }));
    await waitFor(() => expect(m.find("POST", "/api/projects/p1/tasks/a/cancel")).toHaveLength(1));
  });

  it("creates a task; only an owner can switch on automatic merging", async () => {
    const m = mockFetch({ ...routes("member", [task("dep")]), "POST /api/projects/p1/tasks": task("n") });
    await renderApp("/p/p1?tab=tasks");
    fireEvent.click(await page().findByRole("button", { name: /New task/ }));
    const d = within(await screen.findByRole("dialog"));
    // model, budget, attempts, dependencies and merging are behind "Advanced options"
    expect(d.queryByLabelText("Budget (USD)")).toBeNull();
    fireEvent.click(d.getByRole("button", { name: /Advanced options/ }));
    expect(d.getByRole("switch", { name: "Merge automatically" }).hasAttribute("disabled")).toBe(true);
    fireEvent.change(d.getByLabelText("Title"), { target: { value: "Fix flaky test" } });
    fireEvent.change(d.getByLabelText("Spec"), { target: { value: "make it stable" } });
    fireEvent.change(d.getByLabelText("Budget (USD)"), { target: { value: "3" } });
    fireEvent.click(d.getByRole("checkbox", { name: "Task dep" }));
    fireEvent.click(d.getByRole("button", { name: "Create task" }));
    await waitFor(() => expect(m.find("POST", "/api/projects/p1/tasks")).toHaveLength(1));
    expect(m.find("POST", "/api/projects/p1/tasks")[0]?.body).toEqual({ title: "Fix flaky test", spec: "make it stable", budget: { maxUsd: 3 }, dependsOn: ["dep"] });
  });

  it("offers the project's roles on the task dialog and says when one runs on an external executor", async () => {
    const m = mockFetch({
      ...routes("member"),
      "GET /api/projects/p1/roles": [{ scope: "project", scopeId: "p1", name: "fixer", prompt: "x", enabled: true, version: 1, executor: "claude-code", editable: true, createdAt: T, updatedAt: T }],
      "POST /api/projects/p1/tasks": task("n"),
    });
    await renderApp("/p/p1?tab=tasks");
    fireEvent.click(await page().findByRole("button", { name: /New task/ }));
    const d = within(await screen.findByRole("dialog"));
    fireEvent.change(d.getByLabelText("Title"), { target: { value: "Port it" } });
    fireEvent.change(d.getByLabelText("Spec"), { target: { value: "port the module" } });
    fireEvent.click(d.getByRole("combobox", { name: "Role" }));
    fireEvent.click(await screen.findByRole("option", { name: /fixer \(runs on claude-code\)/ }));
    expect(d.getByText(/Runs on the external executor claude-code/)).toBeTruthy();
    fireEvent.click(d.getByRole("button", { name: "Create task" }));
    await waitFor(() => expect(m.find("POST", "/api/projects/p1/tasks")).toHaveLength(1));
    expect(m.find("POST", "/api/projects/p1/tasks")[0]?.body).toMatchObject({ role: "fixer" });
  });

  it("says so when the server has no agent factory", async () => {
    mockFetch({ ...routes(), "GET /api/projects/p1/tasks?limit=500": reply(404, { error: { code: "not_found", message: "no" } }) });
    await renderApp("/p/p1?tab=tasks");
    expect(await page().findByText("The agent factory is not enabled on this server")).toBeTruthy();
  });
});

describe("Triggers and report", () => {
  const trigger = { id: "tr1", owner: "alice", name: "Nightly", kind: "cron", schedule: "0 2 * * *", template: { title: "Check deps", spec: "" }, enabled: true, createdAt: T };

  it("lets an owner create and delete a trigger", async () => {
    const m = mockFetch({ ...routes(), "GET /api/projects/p1/triggers": [trigger], "POST /api/projects/p1/triggers": { ...trigger, id: "tr2" }, "DELETE /api/projects/p1/triggers/tr1": reply(204) });
    await renderApp("/p/p1?tab=tasks");
    fireEvent.mouseDown(await page().findByRole("tab", { name: "Triggers" }));
    expect(await page().findByText("Nightly")).toBeTruthy();
    fireEvent.click(page().getByRole("button", { name: /New trigger/ }));
    const d = within(await screen.findByRole("dialog"));
    fireEvent.change(d.getByLabelText("Name"), { target: { value: "Weekly" } });
    fireEvent.change(d.getByLabelText("Schedule"), { target: { value: "@every 168h" } });
    fireEvent.change(d.getByLabelText("Task title"), { target: { value: "Weekly review" } });
    fireEvent.click(d.getByRole("button", { name: "Create trigger" }));
    await waitFor(() => expect(m.find("POST", "/api/projects/p1/triggers")).toHaveLength(1));
    expect(m.find("POST", "/api/projects/p1/triggers")[0]?.body).toEqual({ name: "Weekly", kind: "cron", enabled: true, schedule: "@every 168h", template: { title: "Weekly review", spec: "" } });
    fireEvent.click(await page().findByRole("button", { name: "Delete Nightly" }));
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));
    await waitFor(() => expect(m.find("DELETE", "/api/projects/p1/triggers/tr1")).toHaveLength(1));
  });

  it("hides the trigger controls from a member", async () => {
    mockFetch({ ...routes("member"), "GET /api/projects/p1/triggers": [trigger] });
    await renderApp("/p/p1?tab=tasks");
    fireEvent.mouseDown(await page().findByRole("tab", { name: "Triggers" }));
    expect(await page().findByText("Nightly")).toBeTruthy();
    expect(page().queryByRole("button", { name: /New trigger/ })).toBeNull();
    expect(page().queryByRole("button", { name: "Delete Nightly" })).toBeNull();
  });

  it("shows the spend with the unpriced warning, and treats a 404 learning view as off", async () => {
    mockFetch({
      ...routes(),
      "GET /api/projects/p1/factory/report?days=7": { ProjectID: "p1", Since: T, Total: { usd: 12.5, tokens: 5000, runs: 4, unpriced: 2 }, ByState: { done: 3, blocked: 1 }, Top: [{ ID: "a", Title: "Big task", State: "done", Spent: { usd: 9, tokens: 1, runs: 1 } }] },
      "GET /api/projects/p1/factory/learning?view=clusters": reply(404),
      "GET /api/projects/p1/factory/learning?view=experiment": reply(404),
    });
    await renderApp("/p/p1?tab=tasks");
    fireEvent.mouseDown(await page().findByRole("tab", { name: "Report" }));
    expect(await page().findByText("$12.50")).toBeTruthy();
    expect(page().getByText(/2 runs used a model with no known price/)).toBeTruthy();
    expect(page().getByText("Big task")).toBeTruthy();
    expect(await page().findByText("Learning views are off on this server.")).toBeTruthy();
  });
});
