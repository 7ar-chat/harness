// The @ picker inside the real composer: typing @ lists roles and skills, picking a role becomes a chip, and the run carries it.
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { Thread } from "@/api/types";
import { mockFetch, reply, type Call } from "@/test/fetch";
import { overview, shellRoutes, thread as summary } from "@/test/fixtures";
import { memoryList, memoryStats } from "@/test/memory";
import { renderApp } from "@/test/render";

const T = "2026-01-01T00:00:00Z";
const idle: Thread = { id: "t1", title: "Docs", pinned: false, status: "idle", createdBy: "alice", createdAt: T, updatedAt: T, activeRun: null, pendingApproval: null, pendingQuestion: null, messages: [] };
const role = (name: string, extra: object = {}) => ({ scope: name === "planner" ? "builtin" : "project", name, title: name, description: `the ${name}`, prompt: "p", enabled: true, version: 1, createdAt: T, updatedAt: T, editable: false, ...extra });

const routes = (extra: Record<string, unknown> = {}) => ({
  ...shellRoutes(overview({ personal: [summary("t1", { title: "Docs" })] })),
  "GET /api/threads/t1": idle,
  "GET /api/models": { models: ["gpt-test"], default: "gpt-test" },
  "GET /api/commands": [
    { name: "review", description: "Review a change", source: "skill" },
    { name: "init", description: "Set up", source: "builtin" },
  ],
  "GET /api/me/roles": [role("planner"), role("docs", { enabled: true }), role("off", { enabled: false })],
  "GET /api/threads/t1/files": [],
  "GET /api/memory/stats": memoryStats(),
  "GET /api/memory": memoryList(),
  "POST /api/threads/t1/runs": { id: "r1", threadId: "t1", status: "running" },
  "GET /api/runs/r1/events": (_c: Call) => reply(200, 'id: 1\ndata: {"type":"RUN_FINISHED","runId":"r1"}\n\n', { headers: { "Content-Type": "text/event-stream" } }),
  ...extra,
});
const page = () => within(screen.getByRole("main"));

describe("the @ picker in the composer", () => {
  it("lists the enabled roles and the installed skills when you type @", async () => {
    mockFetch(routes());
    await renderApp("/t/t1");
    const box = await page().findByRole("textbox", { name: "Message" });

    fireEvent.change(box, { target: { value: "@" } });

    const list = await page().findByRole("listbox", { name: "Mentions" });
    expect(within(list).getByText("@planner")).toBeTruthy();
    expect(within(list).getByText("@docs")).toBeTruthy();
    expect(within(list).getByText("@review")).toBeTruthy();
    expect(within(list).queryByText("@off")).toBeNull();
    expect(within(list).queryByText("@init")).toBeNull();
  });

  it("narrows the list as you type and closes it with Escape", async () => {
    mockFetch(routes());
    await renderApp("/t/t1");
    const box = await page().findByRole("textbox", { name: "Message" });

    fireEvent.change(box, { target: { value: "look at @do" } });
    const list = await page().findByRole("listbox", { name: "Mentions" });
    expect(within(list).getAllByRole("option")).toHaveLength(1);
    fireEvent.keyDown(box, { key: "Escape" });

    await waitFor(() => expect(page().queryByRole("listbox", { name: "Mentions" })).toBeNull());
  });

  it("turns a picked role into a chip and sends the run as that role, with the skills picked", async () => {
    const f = mockFetch(routes());
    await renderApp("/t/t1");
    const box = await page().findByRole("textbox", { name: "Message" });

    fireEvent.change(box, { target: { value: "@do" } });
    fireEvent.click(await page().findByRole("option", { name: /@docs/ }));
    expect(await page().findByText("Run as @docs")).toBeTruthy();
    expect((box as HTMLTextAreaElement).value).toBe("");
    fireEvent.change(box, { target: { value: "@rev" } });
    fireEvent.click(await page().findByRole("option", { name: /@review/ }));
    fireEvent.change(box, { target: { value: "write the readme" } });
    fireEvent.click(page().getByRole("button", { name: /Send/ }));

    await waitFor(() => expect(f.find("POST", "/api/threads/t1/runs")).toHaveLength(1));
    expect(f.find("POST", "/api/threads/t1/runs")[0]?.body).toEqual({ message: "write the readme", model: "gpt-test", interactive: true, mode: "build", role: "docs", skills: ["review"] });
    await waitFor(() => expect(page().queryByText("Run as @docs")).toBeNull());
  });

  it("picks with the keyboard: arrows move, Enter picks instead of sending", async () => {
    const f = mockFetch(routes());
    await renderApp("/t/t1");
    const box = await page().findByRole("textbox", { name: "Message" });

    fireEvent.change(box, { target: { value: "@" } });
    await page().findByRole("listbox", { name: "Mentions" });
    fireEvent.keyDown(box, { key: "ArrowDown" });
    expect(page().getAllByRole("option")[1]?.getAttribute("aria-selected")).toBe("true");
    fireEvent.keyDown(box, { key: "Enter" });

    expect(await page().findByText(/Run as @/)).toBeTruthy();
    expect(f.find("POST", "/api/threads/t1/runs")).toHaveLength(0);
  });

  it("removing the chip sends an ordinary run", async () => {
    const f = mockFetch(routes());
    await renderApp("/t/t1");
    const box = await page().findByRole("textbox", { name: "Message" });
    fireEvent.change(box, { target: { value: "@pl" } });
    fireEvent.click(await page().findByRole("option", { name: /@planner/ }));

    fireEvent.click(await page().findByRole("button", { name: "Remove planner" }));
    fireEvent.change(box, { target: { value: "hello" } });
    fireEvent.click(page().getByRole("button", { name: /Send/ }));

    await waitFor(() => expect(f.find("POST", "/api/threads/t1/runs")).toHaveLength(1));
    expect(f.find("POST", "/api/threads/t1/runs")[0]?.body).toEqual({ message: "hello", model: "gpt-test", interactive: true, mode: "build" });
  });

  it("offers requirements and documents the app already has in its cache and writes them into the message", async () => {
    mockFetch(routes());
    const { qc } = await renderApp("/t/t1");
    qc.setQueryData(["requirements", "p1"], [{ id: "REQ-7", title: "Rate limit logins" }]);
    qc.setQueryData(["documents"], [{ name: "cv.pdf", size: 1, mimeType: "application/pdf", modified: T }]);
    const box = await page().findByRole("textbox", { name: "Message" });

    fireEvent.change(box, { target: { value: "see @" } });
    fireEvent.click(await page().findByRole("option", { name: /REQ-7/ }));

    expect((box as HTMLTextAreaElement).value).toBe("see Requirement REQ-7 (Rate limit logins) ");
    fireEvent.change(box, { target: { value: "and @cv" } });
    fireEvent.click(await page().findByRole("option", { name: /cv.pdf/ }));
    expect((box as HTMLTextAreaElement).value).toBe("and /workspace/documents/cv.pdf ");
  });
});
