import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { mockFetch, reply } from "@/test/fetch";
import { me, overview, project, shellRoutes } from "@/test/fixtures";
import { renderApp } from "@/test/render";
import { parseList, validateRole } from "./Roles";

const T = "2026-01-01T00:00:00Z";
const role = (name: string, extra: object = {}) => ({ scope: "builtin", name, title: name, description: `the ${name}`, prompt: `be the ${name}`, enabled: true, version: 1, builtin: true, createdAt: T, updatedAt: T, editable: false, ...extra });
const docs = role("docs", { scope: "project", scopeId: "p1", builtin: undefined, prompt: "write docs", model: "cheap", editable: true, version: 2, denyTools: ["ping"], skills: ["writing"] });

const routes = (r: "owner" | "member" | "viewer", list: unknown[] = [role("coder"), role("planner", { toolProfile: "read-only" }), docs], extra: Record<string, unknown> = {}) => {
  const p = project("p1", { name: "payments", role: r });
  return {
    ...shellRoutes(overview({ projects: [{ ...p, threads: [] }] }), me({ projects: [p] })),
    "GET /api/projects/p1": p,
    "GET /api/projects/p1/members": [],
    "GET /api/projects/p1/github": reply(404),
    "GET /api/memory/stats": reply(404),
    "GET /api/projects/p1/roles": list,
    "GET /api/commands": [{ name: "writing", description: "Write well", source: "skill" }, { name: "review", description: "Review", source: "skill" }],
    "GET /api/projects/p1/capabilities": [{ name: "github", kind: "catalog", title: "GitHub", description: "", url: "", transport: "http", enabled: true, defaultEnabled: false, installed: true }],
    ...extra,
  };
};
const page = () => within(screen.getByRole("main"));
const dialog = () => within(screen.getByRole("dialog"));

describe("validateRole and parseList", () => {
  it("mirrors the server's rules", () => {
    expect(validateRole({ name: "Docs", prompt: "x", toolProfile: "", permissionMode: "" })).toMatch(/name/);
    expect(validateRole({ name: "docs", prompt: "  ", toolProfile: "", permissionMode: "" })).toMatch(/instructions/);
    expect(validateRole({ name: "docs", prompt: "x", toolProfile: "write", permissionMode: "plan" })).toMatch(/plan mode/);
    expect(validateRole({ name: "docs-2", prompt: "x", toolProfile: "read-only", permissionMode: "plan" })).toBeUndefined();
  });

  it("splits a list on commas and lines without blanks or repeats", () => {
    expect(parseList("a, b\n\n c,a")).toEqual(["a", "b", "c"]);
    expect(parseList("")).toEqual([]);
  });
});

describe("the Roles tab", () => {
  it("lists the builtin roles and the project's, flagged", async () => {
    mockFetch(routes("owner", [role("coder"), role("planner"), docs, { ...docs, name: "tester", scope: "project", overrides: true, enabled: false }]));
    await renderApp("/p/p1?tab=roles");

    const list = await page().findByRole("list", { name: "Roles" });
    const rows = within(list).getAllByRole("listitem");
    expect(rows).toHaveLength(4);
    expect(within(rows[0]!).getByText("Builtin")).toBeTruthy();
    expect(within(rows[2]!).getByText("Project")).toBeTruthy();
    expect(within(rows[2]!).getByText("model cheap · 1 denied · skills writing")).toBeTruthy();
    expect(within(rows[3]!).getByText("overrides the builtin")).toBeTruthy();
    expect(within(rows[3]!).getByText("disabled")).toBeTruthy();
    expect(within(rows[0]!).queryByRole("button", { name: /Delete/ })).toBeNull();
  });

  it("creates a role from the editor with every field", async () => {
    const f = mockFetch(routes("member", undefined, { "POST /api/projects/p1/roles": { ...docs, name: "scribe" } }));
    await renderApp("/p/p1?tab=roles");
    fireEvent.click(await page().findByRole("button", { name: /New role/ }));

    fireEvent.change(dialog().getByLabelText("Name"), { target: { value: "scribe" } });
    fireEvent.change(dialog().getByLabelText("Title"), { target: { value: "Scribe" } });
    fireEvent.change(dialog().getByLabelText("Description"), { target: { value: "writes things" } });
    fireEvent.change(dialog().getByLabelText("Instructions"), { target: { value: "Write clearly." } });
    fireEvent.change(dialog().getByLabelText("Model"), { target: { value: "cheap" } });
    fireEvent.change(dialog().getByLabelText("Allowed tools"), { target: { value: "read_file, search_*" } });
    fireEvent.change(dialog().getByLabelText("Denied tools"), { target: { value: "execute_command" } });
    fireEvent.click(await dialog().findByRole("button", { name: "writing" }));
    fireEvent.click(await dialog().findByRole("button", { name: "github" }));
    fireEvent.click(dialog().getByRole("button", { name: "Create" }));

    await waitFor(() => expect(f.find("POST", "/api/projects/p1/roles")).toHaveLength(1));
    expect(f.find("POST", "/api/projects/p1/roles")[0]?.body).toEqual({
      name: "scribe", title: "Scribe", description: "writes things", prompt: "Write clearly.", model: "cheap", reasoningEffort: "", toolProfile: "", allowTools: ["read_file", "search_*"],
      denyTools: ["execute_command"], skills: ["writing"], mcp: ["github"], permissionMode: "", enabled: true,
    });
  });

  it("will not create a role with a bad name or no instructions", async () => {
    mockFetch(routes("member"));
    await renderApp("/p/p1?tab=roles");
    fireEvent.click(await page().findByRole("button", { name: /New role/ }));

    fireEvent.change(dialog().getByLabelText("Name"), { target: { value: "Bad Name" } });
    expect((dialog().getByRole("button", { name: "Create" }) as HTMLButtonElement).disabled).toBe(true);
    expect(dialog().getByRole("alert").textContent).toMatch(/lowercase/);
    fireEvent.change(dialog().getByLabelText("Name"), { target: { value: "good" } });
    expect(dialog().getByRole("alert").textContent).toMatch(/instructions/);
    fireEvent.change(dialog().getByLabelText("Instructions"), { target: { value: "x" } });
    expect((dialog().getByRole("button", { name: "Create" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("edits a role, sending the version it started from", async () => {
    const f = mockFetch(routes("owner", undefined, { "PUT /api/projects/p1/roles/docs": docs }));
    await renderApp("/p/p1?tab=roles");
    fireEvent.click(await page().findByRole("button", { name: "Edit docs" }));

    expect((dialog().getByLabelText("Name") as HTMLInputElement).disabled).toBe(true);
    fireEvent.change(dialog().getByLabelText("Instructions"), { target: { value: "write better docs" } });
    fireEvent.click(dialog().getByRole("button", { name: "Save" }));

    await waitFor(() => expect(f.find("PUT", "/api/projects/p1/roles/docs")).toHaveLength(1));
    const body = f.find("PUT", "/api/projects/p1/roles/docs")[0]?.body as { prompt: string; version: number; denyTools: string[]; skills: string[] };
    expect(body.prompt).toBe("write better docs");
    expect(body.version).toBe(2);
    expect(body.denyTools).toEqual(["ping"]);
    expect(body.skills).toEqual(["writing"]);
  });

  it("previews the effective prompt of the draft", async () => {
    const f = mockFetch(routes("owner", undefined, {
      "POST /api/projects/p1/roles/preview": { prompt: "## Role: docs\nwrite docs", mode: "plan", notes: ["Runs in plan mode: every tool that could change something is removed."], skills: ["writing"] },
    }));
    await renderApp("/p/p1?tab=roles");
    fireEvent.click(await page().findByRole("button", { name: "Edit docs" }));

    fireEvent.click(dialog().getByRole("button", { name: /Preview/ }));

    const pv = await dialog().findByLabelText("Preview");
    expect(within(pv).getByText(/## Role: docs/)).toBeTruthy();
    expect(within(pv).getByText("plan mode")).toBeTruthy();
    expect(within(pv).getByText(/Runs in plan mode/)).toBeTruthy();
    expect((f.find("POST", "/api/projects/p1/roles/preview")[0]?.body as { prompt: string }).prompt).toBe("write docs");
  });

  it("shows the version history and restores one", async () => {
    const f = mockFetch(routes("owner", undefined, {
      "GET /api/projects/p1/roles/docs/history": [{ version: 1, role: { ...docs, prompt: "first draft" }, changedBy: "bob", at: T }],
      "POST /api/projects/p1/roles/docs/restore": docs,
    }));
    await renderApp("/p/p1?tab=roles");
    fireEvent.click(await page().findByRole("button", { name: "Edit docs" }));

    fireEvent.click(dialog().getByRole("button", { name: /History/ }));
    const hist = await dialog().findByLabelText("Version history");
    expect(await within(hist).findByText("v1")).toBeTruthy();
    expect(within(hist).getByText(/first draft/)).toBeTruthy();
    fireEvent.click(within(hist).getByRole("button", { name: "Restore" }));

    await waitFor(() => expect(f.find("POST", "/api/projects/p1/roles/docs/restore")).toHaveLength(1));
    expect(f.find("POST", "/api/projects/p1/roles/docs/restore")[0]?.body).toEqual({ version: 1 });
  });

  it("opens a builtin read-only and copies it into the project", async () => {
    const f = mockFetch(routes("member", undefined, { "POST /api/projects/p1/roles/coder/duplicate": { ...docs, name: "coder", overrides: true } }));
    await renderApp("/p/p1?tab=roles");

    fireEvent.click(await page().findByRole("button", { name: "View coder" }));
    expect(dialog().getByLabelText("Instructions").matches(":disabled")).toBe(true);
    expect(dialog().queryByRole("button", { name: "Save" })).toBeNull();
    fireEvent.click(dialog().getByRole("button", { name: /Copy to the project/ }));

    await waitFor(() => expect(f.find("POST", "/api/projects/p1/roles/coder/duplicate")).toHaveLength(1));
  });

  it("enables and disables a role from the list, and deletes after confirming", async () => {
    const f = mockFetch(routes("owner", undefined, { "PUT /api/projects/p1/roles/docs": docs, "DELETE /api/projects/p1/roles/docs": reply(204) }));
    await renderApp("/p/p1?tab=roles");

    fireEvent.click(await page().findByRole("switch", { name: "Enable docs" }));
    await waitFor(() => expect(f.find("PUT", "/api/projects/p1/roles/docs")).toHaveLength(1));
    expect((f.find("PUT", "/api/projects/p1/roles/docs")[0]?.body as { enabled: boolean }).enabled).toBe(false);
    fireEvent.click(page().getByRole("button", { name: "Delete docs" }));
    fireEvent.click(within(screen.getByRole("alertdialog")).getByRole("button", { name: "Delete" }));

    await waitFor(() => expect(f.find("DELETE", "/api/projects/p1/roles/docs")).toHaveLength(1));
  });

  it("is read-only for a viewer", async () => {
    mockFetch(routes("viewer", [role("coder"), { ...docs, editable: false }]));
    await renderApp("/p/p1?tab=roles");

    await page().findByRole("list", { name: "Roles" });
    expect(page().queryByRole("button", { name: /New role/ })).toBeNull();
    expect(page().queryByRole("button", { name: /Copy/ })).toBeNull();
    expect(page().queryByRole("button", { name: "Delete docs" })).toBeNull();
    expect(page().getAllByRole("button", { name: /View/ }).length).toBeGreaterThan(0);
  });

  it("says so when the server has no roles", async () => {
    mockFetch(routes("owner", undefined, { "GET /api/projects/p1/roles": reply(404, { error: { code: "not_found", message: "x" } }) }));
    await renderApp("/p/p1?tab=roles");
    expect(await page().findByText("Roles are not available")).toBeTruthy();
  });
});
