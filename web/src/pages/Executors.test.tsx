import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { mockFetch } from "@/test/fetch";
import { me, overview, project, shellRoutes } from "@/test/fixtures";
import { renderApp } from "@/test/render";
import { validateExecutor } from "./Executors";

const T = "2026-01-01T00:00:00Z";
const ex = (name: string, extra: object = {}) => ({
  name, title: name, kind: "builtin", command: `${name} {task}`, binary: name, enabled: true, createdAt: T, updatedAt: T, configured: false, editable: false,
  credentialEnv: ["ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL"], ...extra,
});
const mine = ex("mycli", { kind: "custom", editable: true, credentialEnv: ["MYCLI_KEY"], description: "our agent" });

describe("validateExecutor", () => {
  it("mirrors the server's rules", () => {
    const ok = { name: "my-cli", command: "mycli {task}", binary: "mycli", credentialEnv: ["MY_KEY"] };
    expect(validateExecutor(ok)).toBeUndefined();
    expect(validateExecutor({ ...ok, name: "My CLI" })).toMatch(/name/);
    expect(validateExecutor({ ...ok, command: "mycli" })).toMatch(/\{task\}/);
    expect(validateExecutor({ ...ok, binary: " " })).toMatch(/program/);
    expect(validateExecutor({ ...ok, credentialEnv: ["lower"] })).toMatch(/lower/);
  });
});

describe("the Executors card in Settings", () => {
  const routes = (extra: Record<string, unknown> = {}) => ({
    ...shellRoutes(overview({ projects: [] }), me({ projects: [] })),
    "GET /api/executors": [ex("claude-code", { configured: true }), ex("codex")],
    ...extra,
  });

  it("lists the executors and flags the ones with credentials", async () => {
    mockFetch(routes());
    await renderApp("/profile");
    const list = await screen.findByRole("list", { name: "Executors" });
    const rows = within(list).getAllByRole("listitem");
    expect(rows).toHaveLength(2);
    expect(within(rows[0]!).getByText("credentials set")).toBeTruthy();
    expect(within(rows[1]!).queryByText("credentials set")).toBeNull();
  });

  it("seals credentials: a password field, only the changed values are sent, and they are never shown back", async () => {
    const f = mockFetch(routes({ "PUT /api/me/executors/codex/credentials": ex("codex", { configured: true }) }));
    await renderApp("/profile");
    const list = await screen.findByRole("list", { name: "Executors" });
    fireEvent.click(within(list).getByRole("button", { name: "Credentials for codex" }));

    const key = (await screen.findByLabelText("ANTHROPIC_API_KEY")) as HTMLInputElement;
    expect(key.type).toBe("password");
    expect((screen.getByLabelText("ANTHROPIC_BASE_URL") as HTMLInputElement).type).toBe("url");
    expect((within(list).getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(key, { target: { value: "sk-secret-value" } });
    fireEvent.click(within(list).getByRole("button", { name: "Save" }));

    await waitFor(() => expect(f.find("PUT", "/api/me/executors/codex/credentials")).toHaveLength(1));
    expect(f.find("PUT", "/api/me/executors/codex/credentials")[0]!.body).toEqual({ env: { ANTHROPIC_API_KEY: "sk-secret-value" } });
  });

  it("removes the credentials", async () => {
    const f = mockFetch(routes({ "DELETE /api/me/executors/claude-code/credentials": new Response(null, { status: 204 }) }));
    await renderApp("/profile");
    const list = await screen.findByRole("list", { name: "Executors" });
    fireEvent.click(within(list).getByRole("button", { name: "Credentials for claude-code" }));
    fireEvent.click(await screen.findByRole("button", { name: "Remove my credentials" }));
    await waitFor(() => expect(f.find("DELETE", "/api/me/executors/claude-code/credentials")).toHaveLength(1));
  });

  it("shows nothing when the server has no executors", async () => {
    mockFetch({ ...routes(), "GET /api/executors": new Response(null, { status: 404 }) });
    await renderApp("/profile");
    await screen.findByRole("heading", { name: "Settings" });
    expect(screen.queryByRole("list", { name: "Executors" })).toBeNull();
  });
});

describe("the project's executors in the Roles tab", () => {
  const routes = (r: "owner" | "member", extra: Record<string, unknown> = {}) => {
    const p = project("p1", { name: "payments", role: r });
    return {
      ...shellRoutes(overview({ projects: [{ ...p, threads: [] }] }), me({ projects: [p] })),
      "GET /api/projects/p1": p,
      "GET /api/projects/p1/members": [],
      "GET /api/projects/p1/roles": [{ scope: "builtin", name: "coder", title: "coder", prompt: "x", enabled: true, version: 1, builtin: true, createdAt: T, updatedAt: T, editable: false }],
      "GET /api/projects/p1/executors": [ex("claude-code"), mine],
      ...extra,
    };
  };

  it("lists the project's executors and lets the owner define one", async () => {
    const f = mockFetch(routes("owner", { "POST /api/projects/p1/executors": mine }));
    await renderApp("/p/p1?tab=roles");
    const list = await screen.findByRole("list", { name: "Project executors" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(2);
    expect(within(list).getByText("Project")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: /New executor/ }));
    const dlg = within(await screen.findByRole("dialog"));
    expect((dlg.getByRole("button", { name: "Create" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(dlg.getByLabelText("Name"), { target: { value: "mycli" } });
    fireEvent.change(dlg.getByLabelText("Command"), { target: { value: "mycli run {task}" } });
    fireEvent.change(dlg.getByLabelText("Program"), { target: { value: "mycli" } });
    fireEvent.change(dlg.getByLabelText("Credential variables"), { target: { value: "MYCLI_KEY, MYCLI_KEY" } });
    fireEvent.click(dlg.getByRole("button", { name: "Create" }));

    await waitFor(() => expect(f.find("POST", "/api/projects/p1/executors")).toHaveLength(1));
    expect(f.find("POST", "/api/projects/p1/executors")[0]!.body).toMatchObject({ name: "mycli", command: "mycli run {task}", binary: "mycli", credentialEnv: ["MYCLI_KEY"], enabled: true });
  });

  it("offers the custom credentials with the project in the query, and deletes a definition", async () => {
    const f = mockFetch(routes("owner", { "PUT /api/me/executors/mycli/credentials?project=p1": mine, "DELETE /api/projects/p1/executors/mycli": new Response(null, { status: 204 }) }));
    await renderApp("/p/p1?tab=roles");
    const list = await screen.findByRole("list", { name: "Project executors" });
    fireEvent.click(within(list).getByRole("button", { name: "Credentials for mycli" }));
    fireEvent.change(await screen.findByLabelText("MYCLI_KEY"), { target: { value: "abc-123-secret" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(f.find("PUT", "/api/me/executors/mycli/credentials?project=p1")).toHaveLength(1));

    fireEvent.click(within(list).getByRole("button", { name: "Delete mycli" }));
    await waitFor(() => expect(f.find("DELETE", "/api/projects/p1/executors/mycli")).toHaveLength(1));
  });

  it("does not offer definitions to a viewer of the list who cannot edit", async () => {
    mockFetch(routes("member", { "GET /api/projects/p1/executors": [ex("claude-code"), { ...mine, editable: false }] }));
    await renderApp("/p/p1?tab=roles");
    const list = await screen.findByRole("list", { name: "Project executors" });
    expect(within(list).queryByRole("button", { name: "Delete mycli" })).toBeNull();
    expect(within(list).queryByRole("button", { name: "Edit mycli" })).toBeNull();
  });
});

describe("the executor of a role", () => {
  const T2 = "2026-01-01T00:00:00Z";
  const role = (name: string, extra: object = {}) => ({ scope: "project", scopeId: "p1", name, title: name, prompt: "do it", enabled: true, version: 1, createdAt: T2, updatedAt: T2, editable: true, ...extra });
  const routes = (list: unknown[], extra: Record<string, unknown> = {}) => {
    const p = project("p1", { name: "payments", role: "member" });
    return {
      ...shellRoutes(overview({ projects: [{ ...p, threads: [] }] }), me({ projects: [p] })),
      "GET /api/projects/p1": p,
      "GET /api/projects/p1/members": [],
      "GET /api/projects/p1/roles": list,
      "GET /api/projects/p1/executors": [ex("claude-code"), ex("codex")],
      "GET /api/commands": [],
      "GET /api/projects/p1/capabilities": [],
      ...extra,
    };
  };

  it("shows which executor a role runs on", async () => {
    mockFetch(routes([role("fixer", { executor: "claude-code" })]));
    await renderApp("/p/p1?tab=roles");
    const list = await screen.findByRole("list", { name: "Roles" });
    expect(within(list).getByText(/runs on claude-code/)).toBeTruthy();
  });

  it("sends the chosen executor when the role is saved", async () => {
    const f = mockFetch(routes([role("fixer")], { "PUT /api/projects/p1/roles/fixer": role("fixer", { executor: "codex" }) }));
    await renderApp("/p/p1?tab=roles");
    fireEvent.click(await screen.findByRole("button", { name: "Edit fixer" }));
    const dlg = within(await screen.findByRole("dialog"));
    fireEvent.click(dlg.getByRole("combobox", { name: "Executor" }));
    fireEvent.click(await screen.findByRole("option", { name: "codex" }));
    fireEvent.click(dlg.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(f.find("PUT", "/api/projects/p1/roles/fixer")).toHaveLength(1));
    expect(f.find("PUT", "/api/projects/p1/roles/fixer")[0]!.body).toMatchObject({ name: "fixer", executor: "codex" });
  });
});
