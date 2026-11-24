import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { checkValues, fillTemplate, valuesOf } from "@/api/recipes";
import { mockFetch, reply } from "@/test/fetch";
import { me, overview, project, shellRoutes } from "@/test/fixtures";
import { renderApp } from "@/test/render";

const T = "2026-11-01T00:00:00Z";
const recipe = (over: object = {}) => ({
  id: "weekly-status-digest", name: "Weekly project status digest", summary: "Every Friday, a short status report.", category: "reporting", kind: "schedule",
  body: { name: "Weekly status digest", prompt: "Write the status report for {{audience}}, at most {{length}} lines.", cron: "{{when}}", timezone: "{{timezone}}" },
  params: [
    { key: "when", label: "When to run", type: "cron", default: "0 17 * * 5", required: true },
    { key: "timezone", label: "Time zone", type: "text", default: "UTC", required: true },
    { key: "audience", label: "Who reads it", help: "Sets the tone.", type: "choice", choices: ["the whole team", "managers"], default: "the whole team", required: true },
    { key: "length", label: "Maximum length (lines)", type: "number", default: "20", required: true },
  ],
  suggest: { policy: "readonly", maxUsd: 1, timeoutMinutes: 20 }, version: 1, scope: "builtin", author: "7ar", createdAt: T, updatedAt: T, uses: 0,
  creates: "A schedule: every Friday at 17:00 (UTC)", cost: "About $1.00 or less per run", canEdit: false, ...over,
});
const triage = recipe({
  id: "issue-triage", name: "Issue triage: label new GitHub issues", summary: "Read and label new issues.", category: "maintenance", kind: "trigger",
  body: { name: "Issue triage", event: "issues.opened", title: "Triage: {{.title}}", prompt: "Labels: {{labels}}" }, params: [{ key: "labels", label: "Labels to prefer", type: "text", default: "bug, question", required: true }],
  suggest: { policy: "sandboxed", maxUsd: 0.5 }, creates: "A trigger: starts a task when a GitHub event happens (issues.opened)", cost: "Capped at $0.50 per task",
});
const mine = recipe({ id: "r_1", name: "Release check", summary: "Check the release.", category: "quality", scope: "personal", canEdit: true, uses: 3 });

const shell = (role: "owner" | "member" | "viewer" = "owner") => ({
  ...shellRoutes(overview(), me({ projects: [project("p1", { role })] })),
  "GET /api/projects/p1": project("p1", { role }),
  "GET /api/models": { models: ["gpt-a"], default: "gpt-a" },
  "GET /api/schedules/preview": () => ({ times: ["2026-11-06T17:00:00Z", "2026-11-13T17:00:00Z", "2026-11-20T17:00:00Z"] }),
});
const page = () => within(screen.getByRole("main"));

describe("recipe templating in the browser", () => {
  const params = recipe().params;
  it("fills placeholders in one pass and shows what is missing by its label", () => {
    const v = valuesOf(params, { audience: "managers", length: " " });
    expect(v).toMatchObject({ audience: "managers", length: "20", timezone: "UTC" });
    expect(fillTemplate("for {{audience}} {{ length }} {{nope}}", params, { ...v, length: "{{audience}}" })).toBe("for managers {{audience}} «nope»");
  });

  it("checks types the way the server does", () => {
    expect(checkValues(params, { when: "every day", timezone: "UTC", audience: "x", length: "ten" })).toEqual({ when: "Five fields, for example 0 2 * * *", length: "Must be a number" });
    expect(checkValues(params, { when: "@daily", timezone: "", audience: "x", length: "5" })).toEqual({ timezone: "Required" });
  });
});

describe("the Recipes tab", () => {
  it("shows the gallery as cards and narrows it by category chip and search", async () => {
    mockFetch({ ...shell(), "GET /api/recipes": [recipe(), triage, mine] });
    await renderApp("/p/p1?tab=recipes");

    const list = await page().findByRole("list", { name: "Recipes" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(3);
    const card = within(within(list).getByRole("listitem", { name: "Weekly project status digest" }));
    expect(card.getByText("A schedule: every Friday at 17:00 (UTC)")).toBeTruthy();
    expect(card.getByText("About $1.00 or less per run")).toBeTruthy();

    fireEvent.click(page().getByRole("button", { name: "Maintenance" }));
    expect(within(page().getByRole("list", { name: "Recipes" })).getAllByRole("listitem")).toHaveLength(1);
    fireEvent.click(page().getByRole("button", { name: "All" }));
    fireEvent.change(page().getByLabelText("Search recipes"), { target: { value: "release" } });
    expect(within(page().getByRole("list", { name: "Recipes" })).getAllByRole("listitem")[0]?.textContent).toContain("Release check");
    fireEvent.change(page().getByLabelText("Search recipes"), { target: { value: "zzz" } });
    expect(page().getByText("No recipe matches")).toBeTruthy();
  });

  it("starts a recipe in a short wizard: a live preview, Advanced folded, then one apply call", async () => {
    const m = mockFetch({
      ...shell(),
      "GET /api/recipes": [recipe()],
      "POST /api/projects/p1/recipes/weekly-status-digest/apply": { kind: "schedule", summary: "A schedule: every Friday at 17:00 (Europe/Paris)", recipe: "weekly-status-digest", created: { id: "s9" } },
    });
    await renderApp("/p/p1?tab=recipes");
    fireEvent.click(await page().findByRole("button", { name: "Use this recipe" }));

    const dlg = within(await screen.findByRole("dialog"));
    expect(dlg.getByText("This will create")).toBeTruthy();
    expect(dlg.getByLabelText("What the agent will be told").textContent).toBe("Write the status report for the whole team, at most 20 lines.");
    expect(dlg.queryByLabelText("Budget (USD)")).toBeNull();
    expect(dlg.queryByLabelText("Stop after (minutes)")).toBeNull(); // Advanced is folded

    fireEvent.change(dlg.getByLabelText(/Maximum length/), { target: { value: "8" } });
    fireEvent.change(dlg.getByLabelText(/^Time zone/), { target: { value: "Europe/Paris" } });
    await waitFor(() => expect(dlg.getByLabelText("What the agent will be told").textContent).toContain("at most 8 lines"));
    expect(dlg.getByText(/Next runs/)).toBeTruthy();
    fireEvent.click(dlg.getByRole("button", { name: "Advanced" }));
    expect((dlg.getByLabelText("Stop after (minutes)") as HTMLInputElement).value).toBe("20");

    fireEvent.click(dlg.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(m.find("POST", "/api/projects/p1/recipes/")).toHaveLength(1));
    const body = m.find("POST", "/api/projects/p1/recipes/")[0]!.body as { params: Record<string, string>; advanced: Record<string, unknown> };
    expect(body.params).toMatchObject({ length: "8", timezone: "Europe/Paris", audience: "the whole team", when: "0 17 * * 5" });
    expect(body.advanced).toMatchObject({ policy: "readonly", timeoutMinutes: 20 });
    expect(await dlg.findByText(/Done: Weekly project status digest/)).toBeTruthy();
    expect(dlg.getByRole("link", { name: "Open Schedules" }).getAttribute("href")).toContain("tab=schedules");
  });

  it("keeps Create off while a required value is wrong and shows the server's refusal", async () => {
    mockFetch({
      ...shell(),
      "GET /api/recipes": [recipe()],
      "POST /api/projects/p1/recipes/weekly-status-digest/apply": () => reply(403, { error: { code: "forbidden", message: "You do not have permission to do this." } }),
    });
    await renderApp("/p/p1?tab=recipes");
    fireEvent.click(await page().findByRole("button", { name: "Use this recipe" }));
    const dlg = within(await screen.findByRole("dialog"));
    expect(dlg.queryByLabelText("When to run")).toBeNull(); // a common choice: no raw cron on screen
    fireEvent.click(dlg.getByRole("combobox", { name: "When to run: common choices" }));
    fireEvent.click(await screen.findByRole("option", { name: "Custom" }));
    fireEvent.change(dlg.getByLabelText("When to run"), { target: { value: "every day" } });
    expect(dlg.getByText("Five fields, for example 0 2 * * *")).toBeTruthy();
    expect((dlg.getByRole("button", { name: "Create" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(dlg.getByLabelText("When to run"), { target: { value: "@daily" } });
    fireEvent.click(dlg.getByRole("button", { name: "Create" }));
    expect((await dlg.findByRole("alert")).textContent).toContain("You do not have permission");
  });

  it("offers recipes to members only", async () => {
    mockFetch({ ...shell("viewer"), "GET /api/recipes": [recipe()] });
    await renderApp("/p/p1?tab=recipes");
    expect(((await page().findByRole("button", { name: "Use this recipe" })) as HTMLButtonElement).disabled).toBe(true);
  });

  it("imports a file as is, and shows why the server refused one", async () => {
    const m = mockFetch({
      ...shell(),
      "GET /api/recipes": [recipe()],
      "POST /api/recipes/import?scope=personal": (c: { body: unknown }) => ((c.body as { format?: string }).format === "7ar-recipe" ? { ...mine, name: "Imported" } : reply(400, { error: { code: "invalid_recipe", message: "this is not a recipe file" } })),
    });
    await renderApp("/p/p1?tab=recipes");
    fireEvent.click(await page().findByRole("button", { name: /Import a file/ }));
    const dlg = within(await screen.findByRole("dialog"));
    const pick = (text: string) => fireEvent.change(dlg.getByLabelText("Recipe file"), { target: { files: [new File([text], "x.recipe.json", { type: "application/json" })] } });

    pick("not json");
    expect((await dlg.findByRole("alert")).textContent).toContain("not valid JSON");
    pick(JSON.stringify({ hello: 1 }));
    await waitFor(() => expect(dlg.getByRole("alert").textContent).toContain("this is not a recipe file"));
    pick(JSON.stringify({ format: "7ar-recipe", version: 1, recipe: {} }));
    await waitFor(() => expect(m.find("POST", "/api/recipes/import")).toHaveLength(2));
    expect(m.find("POST", "/api/recipes/import")[1]!.body).toEqual({ format: "7ar-recipe", version: 1, recipe: {} });
  });
});

describe("Add from a recipe", () => {
  it("opens the gallery of schedules on the Schedules tab", async () => {
    mockFetch({ ...shell(), "GET /api/projects/p1/schedules": [], "GET /api/recipes": [recipe(), triage] });
    await renderApp("/p/p1?tab=schedules");
    fireEvent.click(await page().findByRole("button", { name: /Add from a recipe/ }));
    const dlg = within(await screen.findByRole("dialog"));
    expect(await dlg.findByText("Weekly project status digest")).toBeTruthy();
    expect(dlg.queryByText("Issue triage: label new GitHub issues")).toBeNull();
    fireEvent.click(dlg.getByRole("button", { name: "Use this recipe" }));
    expect(await screen.findByText("This will create")).toBeTruthy();
  });

  it("is on the Triggers tab for owners, with the trigger recipes", async () => {
    mockFetch({ ...shell(), "GET /api/projects/p1/tasks": [], "GET /api/projects/p1/triggers": [], "GET /api/recipes": [recipe(), triage] });
    await renderApp("/p/p1?tab=tasks");
    fireEvent.mouseDown(await page().findByRole("tab", { name: "Triggers" }));
    fireEvent.click(await page().findByRole("button", { name: /Add from a recipe/ }));
    const dlg = within(await screen.findByRole("dialog"));
    expect(await dlg.findByText("Issue triage: label new GitHub issues")).toBeTruthy();
    expect(dlg.queryByText("Weekly project status digest")).toBeNull();
  });
});

describe("the trigger form", () => {
  it("sends role, policy, budget and base branch from Advanced", async () => {
    const m = mockFetch({ ...shell(), "GET /api/projects/p1/tasks": [], "GET /api/projects/p1/triggers": [], "POST /api/projects/p1/triggers": { id: "t1" }, "GET /api/projects/p1/roles": [] });
    await renderApp("/p/p1?tab=tasks");
    fireEvent.mouseDown(await page().findByRole("tab", { name: "Triggers" }));
    fireEvent.click(await page().findByRole("button", { name: /New trigger/ }));
    const dlg = within(await screen.findByRole("dialog"));
    fireEvent.change(dlg.getByLabelText("Name"), { target: { value: "Nightly" } });
    fireEvent.change(dlg.getByLabelText("Schedule"), { target: { value: "0 2 * * *" } });
    fireEvent.change(dlg.getByLabelText("Task title"), { target: { value: "Check" } });
    fireEvent.click(dlg.getByRole("button", { name: "Advanced" }));
    fireEvent.change(dlg.getByLabelText("Budget per task (USD)"), { target: { value: "2" } });
    fireEvent.change(dlg.getByLabelText("Base branch"), { target: { value: "develop" } });
    fireEvent.click(dlg.getByRole("button", { name: "Create trigger" }));
    await waitFor(() => expect(m.find("POST", "/api/projects/p1/triggers")).toHaveLength(1));
    expect(m.find("POST", "/api/projects/p1/triggers")[0]!.body).toMatchObject({ name: "Nightly", kind: "cron", schedule: "0 2 * * *", template: { title: "Check", budget: { maxUsd: 2 }, baseBranch: "develop" } });
  });
});

describe("Share as recipe", () => {
  it("turns the selected words of a schedule into a parameter", async () => {
    const sched = { id: "s1", projectId: "p1", owner: "alice", runAs: "alice", name: "PRs of api", prompt: "Summarise merged pull requests of api", cron: "0 7 * * 1", timezone: "UTC", timeoutMinutes: 30, enabled: true, createdAt: T, updatedAt: T, canEdit: true };
    const m = mockFetch({ ...shell(), "GET /api/projects/p1/schedules": [sched], "POST /api/projects/p1/recipes/from": { ...mine, name: "PR summary" } });
    await renderApp("/p/p1?tab=schedules");
    fireEvent.click(await page().findByRole("button", { name: "Share PRs of api as a recipe" }));
    const dlg = within(await screen.findByRole("dialog"));
    const text = dlg.getByLabelText("Text") as HTMLTextAreaElement;
    text.setSelectionRange(text.value.length - 3, text.value.length);
    fireEvent.mouseUp(text);
    fireEvent.click(await dlg.findByRole("button", { name: /Make the selection a parameter/ }));
    fireEvent.change(await dlg.findByLabelText("Label for api"), { target: { value: "Project" } });
    fireEvent.click(dlg.getByRole("button", { name: "Share" }));
    await waitFor(() => expect(m.find("POST", "/api/projects/p1/recipes/from")).toHaveLength(1));
    expect(m.find("POST", "/api/projects/p1/recipes/from")[0]!.body).toMatchObject({
      from: "schedule", id: "s1", scope: "personal", params: [{ key: "api", label: "Project", text: "api", type: "text" }],
    });
  });
});
