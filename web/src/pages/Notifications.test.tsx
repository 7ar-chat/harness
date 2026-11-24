import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { mockFetch } from "@/test/fetch";
import { me, overview, project, shellRoutes } from "@/test/fixtures";
import { renderApp } from "@/test/render";

const T = "2026-11-05T10:00:00Z";
const events = [
  { kind: "task.blocked", label: "A task is blocked", help: "A task stopped.", urgent: true, default: true },
  { kind: "task.done", label: "A task is done or merged", help: "A task finished.", urgent: false, default: false },
];
const chan = (over: object = {}) => ({ id: "c1", kind: "webhook", name: "Ops hook", hint: "hooks.example.com", hasSecret: true, enabled: true, createdAt: T, ...over });
const view = (over: object = {}) => ({
  events, rules: { projectId: "p1", events: {}, dedupeMinutes: 10, digest: false, updatedAt: T }, channels: [chan()], canEdit: true, canSecure: true, ...over,
});
const shell = (role: "owner" | "member" = "owner") => ({
  ...shellRoutes(overview(), me({ projects: [project("p1", { role })] })),
  "GET /api/projects/p1": project("p1", { role }),
});
const page = () => within(screen.getByRole("main"));

describe("Project notifications tab", () => {
  it("shows the channels and the rules matrix, and tests a channel", async () => {
    const m = mockFetch({
      ...shell(), "GET /api/projects/p1/notifications": view(), "GET /api/projects/p1/notifications/log": [],
      "POST /api/projects/p1/notification-channels/c1/test": { channel: "Ops hook", status: "delivered" },
    });
    await renderApp("/p/p1?tab=notifications");
    expect((await page().findAllByText("Ops hook")).length).toBeGreaterThan(0);
    expect(page().getByText("Sends to hooks.example.com")).toBeTruthy();
    expect(page().getByRole("table", { name: "Notification rules" })).toBeTruthy();
    expect(page().getByText("Nothing sent yet")).toBeTruthy();
    fireEvent.click(page().getByRole("button", { name: /Test/ }));
    expect(await page().findByText(/Delivered: the test message arrived/)).toBeTruthy();
    expect(m.find("POST", "/api/projects/p1/notification-channels/c1/test")).toHaveLength(1);
  });

  it("saves a rule when a box is ticked", async () => {
    const m = mockFetch({ ...shell(), "GET /api/projects/p1/notifications": view(), "GET /api/projects/p1/notifications/log": [], "PUT /api/projects/p1/notifications": view() });
    await renderApp("/p/p1?tab=notifications");
    fireEvent.click(await page().findByLabelText("A task is blocked: Ops hook"));
    await waitFor(() => expect(m.find("PUT", "/api/projects/p1/notifications")).toHaveLength(1));
    expect(m.find("PUT", "/api/projects/p1/notifications")[0]?.body).toEqual({
      events: { "task.blocked": { channels: ["c1"], person: true } }, dedupeMinutes: 10, digest: false,
    });
  });

  it("walks through adding a WeCom channel with plain instructions, then tests it", async () => {
    const m = mockFetch({
      ...shell(), "GET /api/projects/p1/notifications": view({ channels: [] }), "GET /api/projects/p1/notifications/log": [],
      "POST /api/projects/p1/notification-channels": chan({ id: "c9", kind: "wecom", name: "Team group" }),
      "POST /api/projects/p1/notification-channels/c9/test": { channel: "Team group", status: "failed", error: "the server answered 500" },
    });
    await renderApp("/p/p1?tab=notifications");
    expect(await page().findByText(/A channel is a place the whole team hears/)).toBeTruthy();
    fireEvent.click(page().getByRole("button", { name: /Add channel/ }));
    const dialog = within(await screen.findByRole("dialog"));
    fireEvent.click(dialog.getByText("WeCom group bot"));
    expect(dialog.getByText(/add a group bot, and copy its webhook address/)).toBeTruthy();
    fireEvent.change(dialog.getByLabelText("Name"), { target: { value: "Team group" } });
    fireEvent.change(dialog.getByLabelText("Bot webhook address"), { target: { value: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=abc" } });
    fireEvent.click(dialog.getByRole("button", { name: /Add and send a test/ }));
    await waitFor(() => expect(m.find("POST", "/api/projects/p1/notification-channels/c9/test")).toHaveLength(1));
    expect(m.find("POST", "/api/projects/p1/notification-channels")[0]?.body).toEqual({
      kind: "wecom", name: "Team group", url: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=abc",
    });
  });

  it("highlights failures in the delivery log and offers the why-not hint", async () => {
    mockFetch({
      ...shell(), "GET /api/projects/p1/notifications": view(),
      "GET /api/projects/p1/notifications/log": [{ id: "l1", projectId: "p1", login: "bob", event: "task.blocked", title: "Task needs you: Ship", channel: "Ops hook (webhook)", status: "failed", error: "the server answered 500", at: T }],
    });
    await renderApp("/p/p1?tab=notifications");
    const log = within(await page().findByRole("table", { name: "Delivery log" }));
    expect(log.getByText("the server answered 500")).toBeTruthy();
    expect(log.getByText("failed").closest("tr")?.className).toContain("bg-destructive");
    expect(page().getByText("Why did I not get a message?")).toBeTruthy();
  });

  it("is read-only for a member: no add button, no log, disabled boxes", async () => {
    mockFetch({ ...shell("member"), "GET /api/projects/p1/notifications": view({ canEdit: false, channels: [chan({ hint: undefined })] }) });
    await renderApp("/p/p1?tab=notifications");
    expect((await page().findAllByText("Ops hook")).length).toBeGreaterThan(0);
    expect(page().queryByRole("button", { name: /Add channel/ })).toBeNull();
    expect(page().queryByRole("table", { name: "Delivery log" })).toBeNull();
    expect((page().getByLabelText("A task is blocked: Ops hook") as HTMLInputElement).disabled).toBe(true);
    expect(page().getByText("Only a project owner changes these.")).toBeTruthy();
  });
});

const mine = (over: object = {}) => ({
  events, effective: { "task.blocked": ["inapp", "telegram"], "task.done": ["inapp"] }, available: ["inapp", "telegram"], email: "alice@example.com",
  quietStart: "", quietEnd: "", timezone: "", digest: false, webhook: { set: false, hasSecret: false }, recent: [], ...over,
});

describe("Personal notifications in Settings", () => {
  it("shows my defaults, saves a change with quiet hours, and sends me a test", async () => {
    const m = mockFetch({
      ...shell(), "GET /api/me/notifications": mine(), "PUT /api/me/notifications": mine(),
      "POST /api/me/notifications/test": { results: [{ channel: "Telegram", status: "delivered" }] },
    });
    await renderApp("/profile");
    const table = within(await screen.findByRole("table", { name: "My notifications" }));
    expect((table.getByLabelText("A task is blocked: Telegram") as HTMLInputElement).checked).toBe(true);
    expect((table.getByLabelText("A task is done or merged: Telegram") as HTMLInputElement).checked).toBe(false);
    expect((table.getByLabelText("A task is blocked: In the app") as HTMLInputElement).disabled).toBe(true);
    fireEvent.click(table.getByLabelText("A task is done or merged: Telegram"));
    fireEvent.click(screen.getByLabelText("Quiet hours"));
    fireEvent.click(screen.getByRole("button", { name: "Save notification settings" }));
    await waitFor(() => expect(m.find("PUT", "/api/me/notifications")).toHaveLength(1));
    const body = m.find("PUT", "/api/me/notifications")[0]?.body as { events: Record<string, string[]>; quietStart: string; quietEnd: string };
    expect(body.events["task.done"]).toEqual(["inapp", "telegram"]);
    expect(body.quietStart).toBe("22:00");
    expect(body.quietEnd).toBe("07:00");
    fireEvent.click(screen.getByRole("button", { name: /Send me a test/ }));
    expect(await screen.findByText("Telegram: delivered")).toBeTruthy();
  });

  it("explains when only the app is set up", async () => {
    mockFetch({ ...shell(), "GET /api/me/notifications": mine({ available: ["inapp"], effective: { "task.blocked": ["inapp"], "task.done": ["inapp"] } }), "POST /api/me/notifications/test": { results: [] } });
    await renderApp("/profile");
    expect(await screen.findByText(/Only the app is set up for you/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /Send me a test/ }));
    expect(await screen.findByText(/nothing to test/)).toBeTruthy();
  });
});
