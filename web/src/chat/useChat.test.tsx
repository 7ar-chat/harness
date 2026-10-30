import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { toast } from "sonner";
import { describe, expect, it, vi } from "vitest";
import type { Thread } from "@/api/types";
import { mockFetch, reply, type Call, type Routes } from "@/test/fetch";
import { useChat, type RunSettings } from "./useChat";

const enc = new TextEncoder();
const T = "2026-01-01T00:00:00Z";
const thread = (extra: Partial<Thread> = {}): Thread => ({ id: "t1", title: "T", pinned: false, messages: [], activeRun: null, pendingApproval: null, pendingQuestion: null, status: "idle", createdAt: T, updatedAt: T, ...extra });
const sse = (events: { id: number; ev: Record<string, unknown> }[]) =>
  new Response(
    new ReadableStream<Uint8Array>({
      start(c) {
        for (const e of events) c.enqueue(enc.encode(`id: ${e.id}\ndata: ${JSON.stringify(e.ev)}\n\n`));
        c.close();
      },
    }),
    { status: 200 },
  );

const settings: RunSettings = { interactive: true, mode: "build", model: "m1" };
function setup(routes: Routes, s: RunSettings = settings) {
  const f = mockFetch(routes);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const W = ({ children }: { children: ReactNode }) => <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  const hook = renderHook(() => useChat("t1", s, () => undefined, { login: "alice" }), { wrapper: W });
  return { f, qc, ...hook };
}

describe("useChat", () => {
  it("shows the persisted conversation and no live run for an idle thread", async () => {
    const { result } = setup({ "GET /api/threads/t1": thread({ messages: [{ id: "u1", role: "user", content: "hi", createdAt: T }, { id: "a1", role: "assistant", content: "hello", createdAt: T }] }) });
    await waitFor(() => expect(result.current.messages).toHaveLength(2));
    expect(result.current.running).toBe(false);
    expect(result.current.interrupt).toBeUndefined();
  });

  it("surfaces a pending approval or question from the thread JSON after a reload", async () => {
    const a = setup({ "GET /api/threads/t1": thread({ status: "waiting_approval", pendingApproval: { id: "ap1", toolName: "bash", toolInputPreview: "ls", toolCallId: "c1" } }) });
    await waitFor(() => expect(a.result.current.interrupt).toMatchObject({ id: "ap1", reason: "tool_approval", approval: { toolName: "bash", toolInputPreview: "ls" }, toolCallId: "c1" }));
    a.unmount();
    const q = setup({ "GET /api/threads/t1": thread({ status: "waiting_input", pendingQuestion: { interruptId: "q1", runId: "r1", toolCallId: "c1", question: "Which?", options: [{ label: "A" }], allowFreeText: false } }) });
    await waitFor(() => expect(q.result.current.interrupt).toMatchObject({ id: "q1", reason: "ask_user", message: "Which?", responseSchema: { options: [{ label: "A" }], allowFreeText: false } }));
  });

  it("send starts a run with the settings, then streams the run's events into the live message", async () => {
    // Arrange
    let after = 0;
    const f = setup({
      "GET /api/threads/t1": thread(),
      "POST /api/threads/t1/runs": { id: "r1", threadId: "t1", status: "running" },
      "GET /api/runs/r1/events": (c: Call) => {
        after = Number(new URL(c.url, "http://x").searchParams.get("after"));
        return sse([
          { id: 1, ev: { type: "RUN_STARTED", runId: "r1" } },
          { id: 2, ev: { type: "TEXT_MESSAGE_CONTENT", messageId: "m1", delta: "Hel" } },
          { id: 3, ev: { type: "TEXT_MESSAGE_CONTENT", messageId: "m1", delta: "lo" } },
        ]);
      },
    });
    await waitFor(() => expect(f.result.current.thread.isSuccess).toBe(true));
    // Act
    await act(async () => {
      await f.result.current.send("do it");
    });
    // Assert
    expect(f.f.find("POST", "/api/threads/t1/runs")[0]?.body).toEqual({ message: "do it", model: "m1", interactive: true, mode: "build" });
    await waitFor(() => expect(f.result.current.live.parts).toEqual([{ kind: "text", id: "m1", text: "Hello" }]));
    expect(after).toBe(0);
    expect(f.result.current.live).toMatchObject({ runId: "r1", phase: "running" });
    expect(f.result.current.messages.at(-1)).toMatchObject({ role: "assistant", live: true, parts: [{ kind: "text", text: "Hello" }] });
  });

  it("when the run finishes it reloads the thread so the persisted messages replace the stream", async () => {
    let threads = 0;
    const f = setup({
      "GET /api/threads/t1": () => (++threads === 1 ? thread() : thread({ messages: [{ id: "u1", runId: "r1", role: "user", content: "do it", createdAt: T }, { id: "a1", runId: "r1", role: "assistant", content: "done", createdAt: T }] })),
      "POST /api/threads/t1/runs": { id: "r1", threadId: "t1", status: "running" },
      "GET /api/runs/r1/events": () =>
        sse([
          { id: 1, ev: { type: "RUN_STARTED", runId: "r1" } },
          { id: 2, ev: { type: "TEXT_MESSAGE_CONTENT", messageId: "m1", delta: "done" } },
          { id: 3, ev: { type: "RUN_FINISHED", outcome: { type: "completed" } } },
        ]),
    });
    await waitFor(() => expect(f.result.current.thread.isSuccess).toBe(true));
    await act(async () => {
      await f.result.current.send("do it");
    });
    await waitFor(() => expect(f.result.current.live.phase).toBe("completed"));
    await waitFor(() => expect(f.result.current.messages.map((m) => m.id)).toEqual(["u1", "a1"]));
    expect(f.result.current.live.parts).toEqual([]);
    expect(f.result.current.running).toBe(false);
  });

  it("follows a run somebody else started, and a send during it steers instead of starting another", async () => {
    const f = setup({
      "GET /api/threads/t1": thread({ activeRun: { id: "r7", status: "running" }, status: "running" }),
      "GET /api/runs/r7/events": () => new Response(new ReadableStream<Uint8Array>({ start() {} }), { status: 200 }),
      "POST /api/runs/r7/steer": {},
    });
    await waitFor(() => expect(f.result.current.running).toBe(true));
    await waitFor(() => expect(f.f.find("GET", "/api/runs/r7/events")).toHaveLength(1));
    await act(async () => {
      await f.result.current.send("also check x");
    });
    expect(f.f.find("POST", "/api/runs/r7/steer")[0]?.body).toEqual({ text: "also check x" });
    expect(f.f.find("POST", "/api/threads/t1/runs")).toHaveLength(0);
  });

  it("cancel posts to the active run, and does nothing when none is active", async () => {
    const f = setup({
      "GET /api/threads/t1": thread({ activeRun: { id: "r7", status: "running" } }),
      "GET /api/runs/r7/events": () => new Response(new ReadableStream<Uint8Array>({ start() {} }), { status: 200 }),
      "POST /api/runs/r7/cancel": {},
    });
    await waitFor(() => expect(f.result.current.running).toBe(true));
    await act(async () => f.result.current.cancel());
    expect(f.f.find("POST", "/api/runs/r7/cancel")).toHaveLength(1);

    const idle = setup({ "GET /api/threads/t1": thread() });
    await waitFor(() => expect(idle.result.current.thread.isSuccess).toBe(true));
    await act(async () => idle.result.current.cancel());
    expect(idle.f.find("POST", "/api/runs/")).toHaveLength(0);
  });

  it("toasts and rethrows when starting a run fails, and clears the pending message", async () => {
    const err = vi.spyOn(toast, "error").mockImplementation(() => "");
    const f = setup({ "GET /api/threads/t1": thread(), "POST /api/threads/t1/runs": reply(429, { error: { code: "busy", message: "Too many runs." } }) });
    await waitFor(() => expect(f.result.current.thread.isSuccess).toBe(true));
    let thrown: unknown;
    await act(async () => {
      thrown = await f.result.current.send("x").catch((e: unknown) => e);
    });
    expect(thrown).toMatchObject({ status: 429 });
    expect(err).toHaveBeenCalledWith("Too many runs.");
    expect(f.result.current.messages).toEqual([]);
    expect(f.result.current.busy).toBe(false);
  });

  it("resume answers the interrupt with the current settings and follows the new run", async () => {
    const f = setup({
      "GET /api/threads/t1": thread({ status: "waiting_approval", pendingApproval: { id: "ap1", toolName: "bash" } }),
      "POST /api/threads/t1/resume": { id: "r2", threadId: "t1", status: "running" },
      "GET /api/runs/r2/events": () => new Response(new ReadableStream<Uint8Array>({ start() {} }), { status: 200 }),
    });
    await waitFor(() => expect(f.result.current.interrupt).toBeDefined());
    await act(async () => {
      await f.result.current.resume({ interruptId: "ap1", reason: "tool_approval", decision: "approve" });
    });
    expect(f.f.find("POST", "/api/threads/t1/resume")[0]?.body).toEqual({ interruptId: "ap1", reason: "tool_approval", decision: "approve", model: "m1", interactive: true, mode: "build" });
    await waitFor(() => expect(f.f.find("GET", "/api/runs/r2/events")).toHaveLength(1));
    expect(f.result.current.live.runId).toBe("r2");
  });

  it("skips undecodable events and keeps the ones around them", async () => {
    const f = setup({
      "GET /api/threads/t1": thread({ activeRun: { id: "r1", status: "running" } }),
      "GET /api/runs/r1/events": () =>
        new Response(
          new ReadableStream<Uint8Array>({
            start(c) {
              c.enqueue(enc.encode('id: 1\ndata: {"type":"TEXT_MESSAGE_CONTENT","messageId":"m","delta":"a"}\n\nid: 2\ndata: {not json\n\nid: 3\ndata: {"type":"TEXT_MESSAGE_CONTENT","messageId":"m","delta":"b"}\n\n'));
            },
          }),
          { status: 200 },
        ),
    });
    await waitFor(() => expect(f.result.current.live.parts).toEqual([{ kind: "text", id: "m", text: "ab" }]));
  });
});
