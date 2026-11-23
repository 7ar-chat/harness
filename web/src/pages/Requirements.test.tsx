import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { mockFetch, reply } from "@/test/fetch";
import { me, overview, project, shellRoutes } from "@/test/fixtures";
import { renderApp } from "@/test/render";

const T = "2026-11-05T10:00:00Z";
const req = (id: string, over: object = {}) => ({ id, projectId: "p1", title: `Req ${id}`, status: "backlog", source: { kind: "manual" }, createdAt: T, updatedAt: T, ...over });
const page = () => within(screen.getByRole("main"));

const routes = (role: "owner" | "member" | "viewer", list: unknown, extra: Record<string, unknown> = {}) => {
  const p = project("p1", { name: "payments", role });
  return {
    ...shellRoutes(overview({ projects: [{ ...p, threads: [] }] }), me({ projects: [p] })),
    "GET /api/projects/p1": p,
    "GET /api/projects/p1/members": [],
    "GET /api/projects/p1/github": { configured: true, connected: true, repo: "acme/payments" },
    "GET /api/projects/p1/requirements": list,
    "GET /api/projects/p1/requirements/settings": { projectId: "p1", autoDispatch: false },
    ...extra,
  };
};

const items = [
  req("a", { title: "Dark mode", status: "ready", labels: ["ui"], priority: 2 }),
  req("b", { title: "Billing export", status: "in_progress", flag: "task failed: tests", pr: { number: 12, url: "https://github.com/acme/payments/pull/12", state: "open" } }),
  req("c", { title: "Old idea", status: "done" }),
];

describe("Requirements tab", () => {
  it("lists requirements with a count per status and filters by status and search", async () => {
    mockFetch(routes("member", items));
    await renderApp("/p/p1?tab=requirements");
    const list = await page().findByRole("list", { name: "Requirements" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(3);
    expect(within(list).getByText("needs attention")).toBeTruthy();

    fireEvent.click(page().getByRole("button", { name: /^In progress\s*\d/ }));
    expect(within(list).getAllByRole("listitem")).toHaveLength(1);
    expect(within(list).getByText("Billing export")).toBeTruthy();

    fireEvent.click(page().getByRole("button", { name: /^All/ }));
    fireEvent.change(page().getByLabelText("Search requirements"), { target: { value: "dark" } });
    expect(within(list).getAllByRole("listitem")).toHaveLength(1);
    expect(within(list).getByText("Dark mode")).toBeTruthy();
  });

  it("shows the linked tasks, sessions and pull request of the selected requirement and dispatches it", async () => {
    const detail = {
      requirement: items[0],
      tasks: [{ id: "k1", title: "Dark mode task", state: "running" }],
      threads: [{ id: "t9", title: "Dark mode chat", status: "idle" }],
    };
    const m = mockFetch(routes("member", items, { "GET /api/projects/p1/requirements/a": detail, "POST /api/projects/p1/requirements/a/dispatch": { ...items[0], status: "in_progress" } }));
    await renderApp("/p/p1?tab=requirements");
    fireEvent.click(await page().findByRole("button", { name: /Dark mode/ }));
    const panel = await page().findByRole("region", { name: "Requirement detail" });
    expect(await within(panel).findByText("Dark mode task")).toBeTruthy();
    expect(within(panel).getByRole("link", { name: /Dark mode chat/ }).getAttribute("href")).toBe("/#/t/t9");
    fireEvent.click(within(panel).getByRole("button", { name: "Dispatch" }));
    await waitFor(() => expect(m.find("POST", "/api/projects/p1/requirements/a/dispatch")).toHaveLength(1));
  });

  it("shows the flag and the pull request of a requirement in progress and does not offer to dispatch it again", async () => {
    const detail = { requirement: items[1], tasks: [{ id: "k2", title: "Billing task", state: "failed", reason: "tests" }], threads: [] };
    mockFetch(routes("member", items, { "GET /api/projects/p1/requirements/b": detail }));
    await renderApp("/p/p1?tab=requirements");
    fireEvent.click(await page().findByRole("button", { name: /Billing export/ }));
    const panel = await page().findByRole("region", { name: "Requirement detail" });
    expect((await within(panel).findByRole("alert")).textContent).toContain("task failed: tests");
    expect(within(panel).getByRole("link", { name: /Pull request #12/ }).getAttribute("href")).toBe("https://github.com/acme/payments/pull/12");
    // a flagged requirement may be dispatched again
    expect((within(panel).getByRole("button", { name: "Dispatch" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("creates a requirement from the dialog", async () => {
    const m = mockFetch(routes("member", [], { "POST /api/projects/p1/requirements": req("n", { title: "Export CSV" }) }));
    await renderApp("/p/p1?tab=requirements");
    expect(await page().findByText("No requirements yet")).toBeTruthy();
    fireEvent.click(page().getByRole("button", { name: /New requirement/ }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.change(within(dialog).getByLabelText("Title"), { target: { value: "Export CSV" } });
    fireEvent.change(within(dialog).getByLabelText(/Description/), { target: { value: "As a user" } });
    fireEvent.change(within(dialog).getByLabelText("Labels"), { target: { value: "api, api, export" } });
    fireEvent.change(within(dialog).getByLabelText("Priority"), { target: { value: "3" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    await waitFor(() => expect(m.find("POST", "/api/projects/p1/requirements")).toHaveLength(1));
    expect(m.find("POST", "/api/projects/p1/requirements")[0]?.body).toEqual({ title: "Export CSV", body: "As a user", priority: 3, labels: ["api", "export"], status: "backlog" });
  });

  it("imports the open issues of the connected repository, with a label", async () => {
    const m = mockFetch(routes("member", items, { "POST /api/projects/p1/requirements/import/github": { imported: 2, skipped: 1, requirements: [] } }));
    await renderApp("/p/p1?tab=requirements");
    fireEvent.change(await page().findByLabelText("Import label"), { target: { value: "bug" } });
    fireEvent.click(page().getByRole("button", { name: "Import from GitHub" }));
    await waitFor(() => expect(m.find("POST", "/api/projects/p1/requirements/import/github")).toHaveLength(1));
    expect(m.find("POST", "/api/projects/p1/requirements/import/github")[0]?.body).toEqual({ label: "bug" });
    expect((await page().findByRole("status")).textContent).toContain("Imported 2");
  });

  it("gives a viewer the list only, and says so when the server does not track requirements", async () => {
    mockFetch(routes("viewer", items));
    await renderApp("/p/p1?tab=requirements");
    await page().findByRole("list", { name: "Requirements" });
    expect(page().queryByRole("button", { name: /New requirement/ })).toBeNull();
    expect(page().queryByRole("switch")).toBeNull();
  });

  it("explains that requirements are off when the route is absent", async () => {
    mockFetch(routes("owner", reply(404)));
    await renderApp("/p/p1?tab=requirements");
    expect(await page().findByText("Requirements are not enabled")).toBeTruthy();
  });

  it("lets an owner turn auto-dispatch on only with a budget", async () => {
    const m = mockFetch(routes("owner", items, { "PUT /api/projects/p1/requirements/settings": { projectId: "p1", autoDispatch: true } }));
    await renderApp("/p/p1?tab=requirements");
    fireEvent.click(await page().findByRole("switch", { name: "Auto-dispatch ready requirements" }));
    const save = within(page().getByRole("region", { name: "Auto-dispatch" })).getByRole("button", { name: "Save" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    fireEvent.change(page().getByLabelText(/Budget per task/), { target: { value: "2.5" } });
    expect(save.disabled).toBe(false);
    fireEvent.click(save);
    await waitFor(() => expect(m.find("PUT", "/api/projects/p1/requirements/settings")).toHaveLength(1));
    expect(m.find("PUT", "/api/projects/p1/requirements/settings")[0]?.body).toEqual({ autoDispatch: true, policy: "sandboxed", budget: { maxUsd: 2.5 } });
  });

  it("changes the status of a requirement from its panel", async () => {
    const m = mockFetch(
      routes("member", items, {
        "GET /api/projects/p1/requirements/a": { requirement: items[0], tasks: [], threads: [] },
        "PATCH /api/projects/p1/requirements/a": { ...items[0], status: "rejected" },
      }),
    );
    await renderApp("/p/p1?tab=requirements");
    fireEvent.click(await page().findByRole("button", { name: /Dark mode/ }));
    const panel = await page().findByRole("region", { name: "Requirement detail" });
    fireEvent.change(within(panel).getByLabelText("Status"), { target: { value: "rejected" } });
    await waitFor(() => expect(m.find("PATCH", "/api/projects/p1/requirements/a")).toHaveLength(1));
    expect(m.find("PATCH", "/api/projects/p1/requirements/a")[0]?.body).toEqual({ status: "rejected" });
  });

  it("names the requirement on the board card of a session that works on it", async () => {
    const card = { threadId: "t1", title: "Add export", status: "idle", updatedAt: T, requirementId: "a", requirementTitle: "Dark mode" };
    mockFetch(routes("member", items, { "GET /api/projects/p1/board": { columns: [{ id: "ready", title: "Ready", cards: [card] }] } }));
    await renderApp("/p/p1");
    expect(await page().findByText("Requirement: Dark mode")).toBeTruthy();
  });
});
