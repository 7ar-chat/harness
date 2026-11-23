import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { mockFetch, reply } from "@/test/fetch";
import { me, overview, project, shellRoutes } from "@/test/fixtures";
import { renderApp } from "@/test/render";
import { formatIn } from "./Schedules";

const T = "2026-11-05T10:00:00Z";
const usage = { inputTokens: 1000, outputTokens: 200, cacheReadTokens: 600, cacheWriteTokens: 100, costUsd: 0.0123 };
const run = (over: object = {}) => ({ id: "r1", scheduleId: "s1", fireAt: T, trigger: "schedule", status: "succeeded", threadId: "th9", startedBy: "alice", startedAt: T, finishedAt: "2026-11-05T10:02:00Z", deadline: T, usage, ...over });
const sched = (over: object = {}) => ({
  id: "s1", projectId: "p1", owner: "alice", runAs: "alice", name: "Morning triage", prompt: "Summarise the open pull requests", cron: "0 9 * * 1-5", timezone: "Europe/Paris",
  timeoutMinutes: 30, enabled: true, nextRunAt: new Date(Date.now() + 3600_000).toISOString(), lastRunAt: T, lastStatus: "succeeded", createdAt: T, updatedAt: T, canEdit: true, lastRun: run(), ...over,
});
const shell = (role: "owner" | "member" | "viewer" = "owner") => ({
  ...shellRoutes(overview(), me({ projects: [project("p1", { role })] })),
  "GET /api/projects/p1": project("p1", { role }),
  "GET /api/models": { models: ["gpt-a", "gpt-b"], default: "gpt-a" },
});
const page = () => within(screen.getByRole("main"));

describe("Schedules tab", () => {
  it("lists the schedules with their next run, last run and who they run as", async () => {
    mockFetch({ ...shell(), "GET /api/projects/p1/schedules": [sched()] });
    await renderApp("/p/p1?tab=schedules");
    expect(await page().findByText("Morning triage")).toBeTruthy();
    expect(page().getByText("0 9 * * 1-5", { exact: false })).toBeTruthy();
    expect(page().getByText(/as alice/)).toBeTruthy();
    expect(page().getByRole("link", { name: "Open the last session" }).getAttribute("href")).toContain("/t/th9");
    expect(page().getByLabelText("Enable Morning triage")).toBeTruthy();
  });

  it("creates a schedule from a preset and shows the next five fire times of the expression", async () => {
    const m = mockFetch({
      ...shell(),
      "GET /api/projects/p1/schedules": [],
      "POST /api/projects/p1/schedules": sched({ id: "s2", name: "Standup" }),
      "GET /api/schedules/preview": () => ({ times: ["2026-11-06T08:00:00Z", "2026-11-09T08:00:00Z", "2026-11-10T08:00:00Z", "2026-11-11T08:00:00Z", "2026-11-12T08:00:00Z"] }),
    });
    await renderApp("/p/p1?tab=schedules");
    fireEvent.click(await page().findByRole("button", { name: /New schedule/ }));
    const dialog = within(await screen.findByRole("dialog"));
    fireEvent.change(dialog.getByLabelText("Name"), { target: { value: "Standup" } });
    fireEvent.change(dialog.getByLabelText("Prompt"), { target: { value: "What changed since yesterday?" } });
    fireEvent.change(dialog.getByLabelText("Cron or interval"), { target: { value: "0 8 * * 1-5" } });
    fireEvent.change(dialog.getByLabelText("Time zone"), { target: { value: "Europe/Paris" } });
    expect(await dialog.findByLabelText("Next runs")).toBeTruthy();
    await waitFor(() => expect(dialog.getByLabelText("Next runs").querySelectorAll("li")).toHaveLength(5));
    const previewCall = m.calls.filter((c) => c.url.startsWith("/api/schedules/preview")).at(-1)!;
    expect(previewCall.url).toContain("cron=0%208%20*%20*%201-5");
    expect(previewCall.url).toContain("timezone=Europe%2FParis");
    fireEvent.click(dialog.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(m.find("POST", "/api/projects/p1/schedules")).toHaveLength(1));
    expect(m.find("POST", "/api/projects/p1/schedules")[0]?.body).toMatchObject({
      name: "Standup", prompt: "What changed since yesterday?", cron: "0 8 * * 1-5", timezone: "Europe/Paris", timeoutMinutes: 30, policy: "default", enabled: true, reuseSession: false,
    });
    // The client never sends an identity: the server decides who it runs as.
    expect(JSON.stringify(m.find("POST", "/api/projects/p1/schedules")[0]?.body)).not.toMatch(/runAs|owner/);
  });

  it("shows why an expression is refused and keeps Create from being sent with a timeout out of range", async () => {
    mockFetch({
      ...shell(),
      "GET /api/projects/p1/schedules": [],
      "GET /api/schedules/preview": () => reply(400, { error: { code: "invalid_schedule", message: "schedule \"nope\": want 5 fields" } }),
    });
    await renderApp("/p/p1?tab=schedules");
    fireEvent.click(await page().findByRole("button", { name: /New schedule/ }));
    const dialog = within(await screen.findByRole("dialog"));
    fireEvent.change(dialog.getByLabelText("Name"), { target: { value: "x" } });
    fireEvent.change(dialog.getByLabelText("Prompt"), { target: { value: "y" } });
    fireEvent.change(dialog.getByLabelText("Cron or interval"), { target: { value: "nope" } });
    expect(await dialog.findByText(/want 5 fields/)).toBeTruthy();
    fireEvent.change(dialog.getByLabelText("Stop after (minutes)"), { target: { value: "500" } });
    expect((dialog.getByRole("button", { name: "Create" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("toggles enabled, runs now and deletes after a confirmation", async () => {
    const m = mockFetch({
      ...shell(),
      "GET /api/projects/p1/schedules": [sched()],
      "PATCH /api/projects/p1/schedules/s1": sched({ enabled: false }),
      "POST /api/projects/p1/schedules/s1/run": run({ status: "running", trigger: "manual" }),
      "DELETE /api/projects/p1/schedules/s1": reply(204),
    });
    await renderApp("/p/p1?tab=schedules");
    fireEvent.click(await page().findByLabelText("Enable Morning triage"));
    await waitFor(() => expect(m.find("PATCH", "/api/projects/p1/schedules/s1")[0]?.body).toEqual({ enabled: false }));
    fireEvent.click(page().getByRole("button", { name: "Run Morning triage now" }));
    await waitFor(() => expect(m.find("POST", "/api/projects/p1/schedules/s1/run")).toHaveLength(1));
    fireEvent.click(page().getByRole("button", { name: "Delete Morning triage" }));
    expect(m.find("DELETE", "/api/projects/p1/schedules/s1")).toHaveLength(0);
    fireEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(m.find("DELETE", "/api/projects/p1/schedules/s1")).toHaveLength(1));
  });

  it("shows the run history with a link to each session", async () => {
    mockFetch({
      ...shell(),
      "GET /api/projects/p1/schedules": [sched()],
      "GET /api/projects/p1/schedules/s1/runs": [run(), run({ id: "r2", status: "timeout", threadId: "th8", error: "the run was cancelled after 30m0s", trigger: "manual" })],
    });
    await renderApp("/p/p1?tab=schedules");
    fireEvent.click(await page().findByRole("button", { name: "Show runs of Morning triage" }));
    const table = within(await page().findByRole("table", { name: "Runs of Morning triage" }));
    expect(table.getByText("timeout")).toBeTruthy();
    expect(table.getByText(/cancelled after/)).toBeTruthy();
    expect(table.getAllByRole("link", { name: "Open session" }).map((a) => a.getAttribute("href"))).toEqual([expect.stringContaining("/t/th9"), expect.stringContaining("/t/th8")]);
    expect(table.getAllByText("2 min")).toHaveLength(2);
  });

  it("is read-only for a viewer", async () => {
    mockFetch({ ...shell("viewer"), "GET /api/projects/p1/schedules": [sched({ canEdit: false })] });
    await renderApp("/p/p1?tab=schedules");
    expect(await page().findByText("Morning triage")).toBeTruthy();
    expect(page().queryByRole("button", { name: /New schedule/ })).toBeNull();
    expect(page().queryByRole("button", { name: "Run Morning triage now" })).toBeNull();
    expect(page().queryByLabelText("Enable Morning triage")).toBeNull();
  });

  it("keeps the personal page at /schedules", async () => {
    const m = mockFetch({ ...shellRoutes(), "GET /api/me/schedules": [] });
    await renderApp("/schedules");
    expect(await page().findByText("No schedules yet")).toBeTruthy();
    expect(m.find("GET", "/api/me/schedules")).toHaveLength(1);
  });

  it("says so when the server has no scheduled tasks", async () => {
    mockFetch({ ...shell(), "GET /api/projects/p1/schedules": reply(404, { error: { code: "not_found", message: "no" } }) });
    await renderApp("/p/p1?tab=schedules");
    expect(await page().findByText("This server has no scheduled tasks.")).toBeTruthy();
  });
});

describe("formatIn", () => {
  it("reads a moment in the zone it is asked for, and survives an unknown one", () => {
    expect(formatIn("2026-11-06T08:00:00Z", "Asia/Tokyo")).toContain("17:00");
    expect(formatIn("2026-11-06T08:00:00Z", "Mars/Base")).toBeTruthy();
  });
});
