import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { CapabilityView } from "@/api/types";
import { mockFetch, reply } from "@/test/fetch";
import { me, overview, project, shellRoutes } from "@/test/fixtures";
import { renderApp } from "@/test/render";

const cap = (name: string, extra: Partial<CapabilityView> = {}): CapabilityView => ({
  name,
  kind: "catalog",
  title: name,
  description: `${name} tools`,
  url: `https://mcp.example.com/${name}`,
  transport: "http",
  enabled: false,
  defaultEnabled: false,
  installed: false,
  ...extra,
});

const github = cap("github", {
  title: "GitHub",
  description: "Issues and pull requests",
  category: "Code",
  homepage: "https://github.com",
  fields: [{ key: "token", label: "Access token", secret: true, required: true, set: false }],
  missing: ["token"],
});
const docs = cap("docs", { title: "Docs search", description: "Search documentation", category: "Knowledge", enabled: true, defaultEnabled: true });
const linear = cap("linear", { title: "Linear", description: "Issue tracking", category: "Code", enabled: true, installed: true, fields: [{ key: "key", label: "API key", secret: true, required: true, set: true }] });
const mine = cap("mine", { kind: "custom", title: "mine", description: "Custom MCP server", enabled: true, installed: true, headerNames: ["Authorization"] });

const routes = (role: "owner" | "member", list: unknown = [github, docs, linear, mine], extra: Record<string, unknown> = {}) => {
  const p = project("p1", { name: "payments", role });
  return {
    ...shellRoutes(overview({ projects: [{ ...p, threads: [] }] }), me({ projects: [p] })),
    "GET /api/projects/p1": p,
    "GET /api/projects/p1/members": [],
    "GET /api/projects/p1/github": reply(404),
    "GET /api/memory/stats": reply(404),
    "GET /api/projects/p1/capabilities": list,
    ...extra,
  };
};
const open = async (role: "owner" | "member", list?: unknown, extra?: Record<string, unknown>) => {
  const m = mockFetch(routes(role, list, extra));
  await renderApp("/p/p1");
  const tab = await screen.findByRole("tab", { name: "Capabilities" });
  fireEvent.mouseDown(tab, { button: 0 });
  fireEvent.click(tab);
  return m;
};
const card = async (title: string) => within((await screen.findByRole("heading", { name: title })).closest("[data-slot=card]") as HTMLElement);

describe("CapabilitiesPanel", () => {
  it("renders the catalog with a status per entry and the custom servers apart", async () => {
    await open("owner");
    const g = await card("GitHub");
    expect(g.getByText("Needs setup")).toBeTruthy();
    expect(g.getByText("Needs: Access token")).toBeTruthy();
    expect(g.getByText("Code")).toBeTruthy();
    expect(g.getByRole("link", { name: "GitHub homepage" }).getAttribute("href")).toBe("https://github.com");
    expect((await card("Docs search")).getByText("On by default")).toBeTruthy();
    expect((await card("Linear")).getByText("On")).toBeTruthy();
    expect(within(screen.getByRole("list", { name: "Custom servers" })).getByText("mine", { selector: "h3" })).toBeTruthy();
    // the search and category filter narrow the marketplace
    fireEvent.change(screen.getByRole("searchbox", { name: "Search the marketplace" }), { target: { value: "documentation" } });
    expect(screen.queryByRole("heading", { name: "GitHub" })).toBeNull();
    expect(screen.getByRole("heading", { name: "Docs search" })).toBeTruthy();
  });

  it("will not enable an entry before its required field is set and says which", async () => {
    const m = await open("owner");
    fireEvent.click((await screen.findAllByRole("button", { name: "Configure GitHub" }))[0]!);
    const dlg = within(await screen.findByRole("dialog"));
    fireEvent.click(dlg.getByRole("switch", { name: "Enabled" }));
    expect((await dlg.findByRole("alert")).textContent).toBe("Set Access token before enabling.");
    expect(dlg.getByRole("switch", { name: "Enabled" }).getAttribute("aria-checked")).toBe("false");
    expect(m.find("PUT", "/api/projects/p1/capabilities")).toHaveLength(0);
  });

  it("sends a secret as `fields` and never shows it back", async () => {
    const saved = { ...github, installed: true, enabled: true, missing: undefined, fields: [{ ...github.fields![0]!, set: true }] };
    let current: CapabilityView[] = [github, docs];
    const m = await open("owner", undefined, {
      "GET /api/projects/p1/capabilities": () => current,
      "PUT /api/projects/p1/capabilities/github": () => {
        current = [saved, docs];
        return saved;
      },
    });
    fireEvent.click((await screen.findAllByRole("button", { name: "Configure GitHub" }))[0]!);
    let dlg = within(await screen.findByRole("dialog"));
    const input = dlg.getByLabelText("Access token") as HTMLInputElement;
    expect(input.type).toBe("password");
    fireEvent.change(input, { target: { value: "ghp_secret" } });
    fireEvent.click(dlg.getByRole("switch", { name: "Enabled" }));
    fireEvent.click(dlg.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(m.find("PUT", "/api/projects/p1/capabilities/github")).toHaveLength(1));
    expect(m.find("PUT", "/api/projects/p1/capabilities/github")[0]!.body).toEqual({ enabled: true, fields: { token: "ghp_secret" } });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect((await card("GitHub")).getByText("On")).toBeTruthy();

    // reopened: only "set" is shown, the value is gone from the page
    fireEvent.click(screen.getAllByRole("button", { name: "Configure GitHub" })[0]!);
    dlg = within(await screen.findByRole("dialog"));
    expect((dlg.getByLabelText("Access token") as HTMLInputElement).value).toBe("");
    expect(dlg.getByText("Set")).toBeTruthy();
    expect(document.body.textContent).not.toContain("ghp_secret");
  });

  it("test connection lists the tools with a read-only badge, or shows the error", async () => {
    let result: unknown = { ok: true, tools: [{ name: "get_issue", description: "Read an issue", readOnly: true }, { name: "create_issue", description: "Open an issue", readOnly: false }] };
    await open("owner", [linear], { "POST /api/projects/p1/capabilities/linear/test": () => result });
    fireEvent.click(await screen.findByRole("button", { name: "Configure Linear" }));
    const dlg = within(await screen.findByRole("dialog"));
    fireEvent.click(dlg.getByRole("button", { name: /Test connection/ }));
    const ok = within(await dlg.findByRole("status"));
    expect(ok.getByText("get_issue")).toBeTruthy();
    expect(ok.getByText("create_issue")).toBeTruthy();
    expect(ok.getAllByText("read-only")).toHaveLength(1);

    result = { ok: false, error: "could not reach the server" };
    fireEvent.click(dlg.getByRole("button", { name: /Test connection/ }));
    expect((await dlg.findByRole("alert")).textContent).toBe("Connection failed: could not reach the server");
  });

  it("rejects a http:// URL and a bad name before sending anything", async () => {
    const m = await open("owner", []);
    fireEvent.click(await screen.findByRole("button", { name: /Add server/ }));
    const dlg = within(await screen.findByRole("dialog"));
    fireEvent.change(dlg.getByLabelText("Name"), { target: { value: "Bad Name" } });
    fireEvent.change(dlg.getByLabelText("URL"), { target: { value: "http://mcp.example.com" } });
    fireEvent.click(dlg.getByRole("button", { name: "Add server" }));
    expect(await dlg.findByText(/lowercase letter first/)).toBeTruthy();
    expect(dlg.getByText("Only https:// URLs are allowed.")).toBeTruthy();
    fireEvent.change(dlg.getByLabelText("Name"), { target: { value: "a__b" } });
    fireEvent.click(dlg.getByRole("button", { name: "Add server" }));
    expect(await dlg.findByText(/two underscores/)).toBeTruthy();
    expect(m.find("POST", "/api/projects/p1/capabilities")).toHaveLength(0);
  });

  it("adds a custom server with masked headers and a tool allow-list", async () => {
    const m = await open("owner", [], { "POST /api/projects/p1/capabilities": () => reply(201, mine) });
    fireEvent.click(await screen.findByRole("button", { name: /Add server/ }));
    const dlg = within(await screen.findByRole("dialog"));
    fireEvent.change(dlg.getByLabelText("Name"), { target: { value: "mine" } });
    fireEvent.change(dlg.getByLabelText("URL"), { target: { value: "https://mcp.example.com/mcp" } });
    fireEvent.click(dlg.getByRole("button", { name: "Add header" }));
    fireEvent.change(dlg.getByLabelText("Header 1 name"), { target: { value: "Authorization" } });
    const value = dlg.getByLabelText("Header 1 value") as HTMLInputElement;
    expect(value.type).toBe("password");
    fireEvent.change(value, { target: { value: "Bearer abc" } });
    fireEvent.change(dlg.getByLabelText("Allowed tools"), { target: { value: "search" } });
    fireEvent.click(dlg.getByRole("button", { name: "Add tool" }));
    fireEvent.click(dlg.getByRole("button", { name: "Add server" }));
    await waitFor(() => expect(m.find("POST", "/api/projects/p1/capabilities")).toHaveLength(1));
    expect(m.find("POST", "/api/projects/p1/capabilities")[0]!.body).toEqual({
      name: "mine",
      url: "https://mcp.example.com/mcp",
      transport: "http",
      enabled: true,
      headers: { Authorization: "Bearer abc" },
      tools: ["search"],
    });
  });

  it("shows a member the catalog and statuses but no controls", async () => {
    await open("member");
    expect((await card("GitHub")).getByText("Needs setup")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Configure/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /Add server/ })).toBeNull();
  });

  it("says the marketplace is unavailable when the server answers 404", async () => {
    await open("owner", reply(404, { error: { code: "not_found", message: "nope" } }));
    expect(await screen.findByText("Capabilities are not available")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Add server/ })).toBeNull();
  });
});

describe("Profile capabilities", () => {
  it("manages the user's own servers through /api/me/capabilities", async () => {
    const m = mockFetch({ ...shellRoutes(), "GET /api/connectors": [], "GET /api/me/capabilities": [docs] });
    await renderApp("/profile");
    expect(await within(screen.getByRole("main")).findByText("Capabilities", { selector: "[data-slot=card-title]" })).toBeTruthy();
    expect(await screen.findByText("Docs search")).toBeTruthy();
    expect(m.find("GET", "/api/me/capabilities")).toHaveLength(1);
  });
});
