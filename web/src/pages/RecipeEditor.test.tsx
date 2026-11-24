import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { recipeText } from "@/api/recipes";
import { mockFetch, reply } from "@/test/fetch";
import { me, overview, project, shellRoutes } from "@/test/fixtures";
import { renderApp } from "@/test/render";

// The diff view measures text on a canvas, which jsdom lacks: a plain stand-in shows the two texts.
vi.mock("@/chat/DiffBlock", () => ({ DiffBlock: ({ oldText, newText }: { oldText: string; newText: string }) => <pre>{`- ${oldText}\n+ ${newText}`}</pre> }));

const T = "2026-11-01T00:00:00Z";
const mine = (over: object = {}) => ({
  id: "r_1", name: "Release check", summary: "Check the release.", category: "quality", kind: "schedule",
  body: { name: "Check {{thing}}", prompt: "Check {{thing}} and report.", cron: "0 9 * * 1", timezone: "UTC" },
  params: [{ key: "thing", label: "What", type: "text", default: "the build", required: true }],
  suggest: { policy: "readonly" }, version: 2, scope: "personal", scopeId: "alice", author: "alice", createdAt: T, updatedAt: "2026-11-02T10:00:00Z", uses: 0,
  creates: "A schedule: every Monday at 09:00 (UTC)", canEdit: true, ...over,
});
const builtin = { ...mine({ id: "weekly-status-digest", name: "Weekly project status digest", scope: "builtin", canEdit: false }) };
const old = { version: 1, recipe: mine({ version: 1, body: { name: "Check {{thing}}", prompt: "Look at {{thing}}.", cron: "0 9 * * 1", timezone: "UTC" } }), changedBy: "alice", at: T };

const shell = () => ({
  ...shellRoutes(overview(), me({ projects: [project("p1", { role: "owner" })] })),
  "GET /api/projects/p1": project("p1", { role: "owner" }),
  "GET /api/models": { models: ["gpt-a"], default: "gpt-a" },
  "POST /api/recipes/check": () => ({ ok: true, preview: { dryRun: true, kind: "schedule", summary: "A schedule: every Monday", rendered: { kind: "schedule", body: { prompt: "Check the build and report." }, suggest: {} } } }),
});
const card = (name: string) => within(screen.getByRole("main")).getByRole("listitem", { name });

describe("the recipe editor", () => {
  it("offers Edit only where the server says the caller may edit, and Duplicate everywhere", async () => {
    mockFetch({ ...shell(), "GET /api/recipes": [builtin, mine()] });
    await renderApp("/p/p1?tab=recipes");
    await screen.findByRole("list", { name: "Recipes" });
    expect(within(card("Release check")).getByRole("button", { name: "Edit Release check" })).toBeTruthy();
    expect(within(card("Weekly project status digest")).queryByRole("button", { name: /^Edit / })).toBeNull();
    expect(within(card("Weekly project status digest")).getByRole("button", { name: /^Duplicate / })).toBeTruthy();
  });

  it("shows the server's message for a bad draft and keeps Save off", async () => {
    mockFetch({
      ...shell(), "GET /api/recipes": [mine()],
      "POST /api/recipes/check": (c: { body: unknown }) =>
        JSON.stringify(c.body).includes("{{missing}}") ? { ok: false, error: "the text uses {{missing}} but no parameter has that key" } : { ok: true },
    });
    await renderApp("/p/p1?tab=recipes");
    fireEvent.click(await screen.findByRole("button", { name: "Edit Release check" }));
    const dlg = await screen.findByRole("dialog");
    fireEvent.change(within(dlg).getByLabelText("Prompt"), { target: { value: "Check {{missing}}" } });
    expect(await within(dlg).findByText(/no parameter has that key/, {}, { timeout: 3000 })).toBeTruthy();
    expect((within(dlg).getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("saves with the version it loaded and explains a stale conflict", async () => {
    const m = mockFetch({
      ...shell(), "GET /api/recipes": [mine()],
      "PUT /api/recipes/r_1": () => reply(409, { error: { code: "stale", message: "Someone changed this recipe meanwhile. Reload it and try again." } }),
      "GET /api/recipes/r_1": mine({ version: 3, summary: "Newer." }),
    });
    await renderApp("/p/p1?tab=recipes");
    fireEvent.click(await screen.findByRole("button", { name: "Edit Release check" }));
    const dlg = await screen.findByRole("dialog");
    fireEvent.change(within(dlg).getByLabelText("What it does, in one line"), { target: { value: "Mine." } });
    await waitFor(() => expect((within(dlg).getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(false), { timeout: 3000 });
    fireEvent.click(within(dlg).getByRole("button", { name: "Save" }));
    expect(await within(dlg).findByText(/Someone changed this recipe meanwhile/)).toBeTruthy();
    expect((m.find("PUT", "/api/recipes/r_1")[0]!.body as { version: number }).version).toBe(2);
    fireEvent.click(within(dlg).getByRole("button", { name: "Load the latest version" }));
    await waitFor(() => expect((within(dlg).getByLabelText("What it does, in one line") as HTMLInputElement).value).toBe("Newer."));
  });

  it("lists versions, restores one as a new version and diffs two", async () => {
    const m = mockFetch({
      ...shell(), "GET /api/recipes": [mine()],
      "GET /api/recipes/r_1/versions": { current: 2, history: [old] },
      "PUT /api/recipes/r_1": (c: { body: unknown }) => mine({ version: 3, body: (c.body as { body: object }).body }),
    });
    await renderApp("/p/p1?tab=recipes");
    fireEvent.click(await screen.findByRole("button", { name: "Edit Release check" }));
    const dlg = await screen.findByRole("dialog");
    fireEvent.click(within(dlg).getByRole("button", { name: "History" }));
    const list = await within(dlg).findByRole("list", { name: "Versions" });
    await waitFor(() => expect(within(list).getAllByRole("listitem")).toHaveLength(2));
    expect(within(dlg).getByLabelText("Changes from version 1 to version 2")).toBeTruthy();
    fireEvent.click(within(dlg).getByRole("button", { name: "Restore version 1" }));
    await waitFor(() => expect(m.find("PUT", "/api/recipes/r_1")).toHaveLength(1));
    const sent = m.find("PUT", "/api/recipes/r_1")[0]!.body as { version: number; body: { prompt: string } };
    expect([sent.version, sent.body.prompt]).toEqual([2, "Look at {{thing}}."]);
  });

  it("makes a diffable text of a recipe", () => {
    const t = recipeText(mine());
    expect(t).toContain("Name: Release check");
    expect(t).toContain("Parameter thing: What, text, required");
    expect(t.endsWith("Prompt:\nCheck {{thing}} and report.")).toBe(true);
  });
});
