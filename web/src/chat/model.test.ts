import { describe, expect, it } from "vitest";
import type { Message } from "@/api/types";
import { initialLive, type LiveState } from "@/lib/agui";
import { messagesFromLive, messagesFromThread } from "./model";

const msg = (m: Partial<Message> & Pick<Message, "id" | "role">): Message => ({ content: "", createdAt: "2026-01-01T00:00:00Z", ...m });
const call = (id: string, name: string, args = "{}") => ({ id, type: "function", function: { name, arguments: args } });

describe("messagesFromThread", () => {
  it("merges the assistant and tool messages of one turn into a single assistant message", () => {
    // Arrange
    const msgs = [
      msg({ id: "u1", role: "user", content: "fix it", author: { login: "alice" } }),
      msg({ id: "a1", role: "assistant", content: "reading", toolCalls: [call("c1", "read_file", '{"path":"a"}')] }),
      msg({ id: "t1", role: "tool", toolCallId: "c1", content: "file body" }),
      msg({ id: "a2", role: "assistant", content: "done" }),
    ];
    // Act
    const out = messagesFromThread(msgs);
    // Assert
    expect(out).toHaveLength(2);
    expect(out[0]).toMatchObject({ id: "u1", role: "user", author: { login: "alice" }, parts: [{ kind: "text", text: "fix it" }] });
    expect(out[1]?.id).toBe("a1");
    expect(out[1]?.parts).toEqual([
      { kind: "text", id: "a1", text: "reading" },
      { kind: "tool", id: "c1", name: "read_file", args: '{"path":"a"}', output: "", ended: true, result: "file body", isError: false },
      { kind: "text", id: "a2", text: "done" },
    ]);
  });

  it("starts a new assistant message after each user message and marks failed tools and run errors", () => {
    const out = messagesFromThread([
      msg({ id: "u1", role: "user", content: "a" }),
      msg({ id: "a1", role: "assistant", toolCalls: [call("c1", "bash")] }),
      msg({ id: "t1", role: "tool", toolCallId: "c1", content: "boom", error: "exit 1" }),
      msg({ id: "a2", role: "assistant", error: "model failed" }),
      msg({ id: "u2", role: "user", content: "b" }),
      msg({ id: "a3", role: "assistant", content: "ok" }),
    ]);
    expect(out.map((m) => m.id)).toEqual(["u1", "a1", "u2", "a3"]);
    expect(out[1]?.parts).toMatchObject([{ kind: "tool", isError: true, result: "boom" }, { kind: "error", message: "model failed" }]);
  });

  it("ignores a tool result that has no matching call and a tool message before any assistant turn", () => {
    const out = messagesFromThread([msg({ id: "t0", role: "tool", toolCallId: "x", content: "stray" }), msg({ id: "a1", role: "assistant", content: "hi" }), msg({ id: "t1", role: "tool", toolCallId: "nope", content: "stray" })]);
    expect(out).toHaveLength(1);
    expect(out[0]?.parts).toEqual([{ kind: "text", id: "a1", text: "hi" }]);
  });

  it("shows system messages as user messages and flags steers", () => {
    const out = messagesFromThread([msg({ id: "s1", role: "system", content: "note" }), msg({ id: "u1", role: "user", content: "also", metadata: { steer: true } }), msg({ id: "u2", role: "user", content: "plain", metadata: { steer: "yes" } })]);
    expect(out.map((m) => [m.role, m.steer])).toEqual([["user", false], ["user", true], ["user", false]]);
  });

  it("hides the messages of the run that is streaming live, but keeps its prompt unless it is a steer", () => {
    const out = messagesFromThread(
      [
        msg({ id: "u0", role: "user", runId: "r0", content: "old" }),
        msg({ id: "a0", role: "assistant", runId: "r0", content: "old reply" }),
        msg({ id: "u1", role: "user", runId: "r1", content: "prompt" }),
        msg({ id: "a1", role: "assistant", runId: "r1", content: "streaming" }),
        msg({ id: "u2", role: "user", runId: "r1", content: "steer", metadata: { steer: true } }),
      ],
      "r1",
    );
    expect(out.map((m) => m.id)).toEqual(["u0", "a0", "u1"]);
  });
});

describe("messagesFromLive", () => {
  const users = (login: string) => (login === "bob" ? { login: "bob", name: "Bob Roe" } : undefined);
  const live = (parts: LiveState["parts"], extra: Partial<LiveState> = {}): LiveState => ({ ...initialLive, runId: "r1", phase: "running", parts, ...extra });

  it("is empty when idle with nothing streamed", () => {
    expect(messagesFromLive(initialLive, false, users)).toEqual([]);
  });

  it("shows a pending assistant bubble while a run has produced nothing yet", () => {
    const out = messagesFromLive(live([]), true, users);
    expect(out).toEqual([{ id: "r1-pending", role: "assistant", parts: [], live: true }]);
  });

  it("puts all parts of a run into one live assistant message", () => {
    const out = messagesFromLive(live([{ kind: "text", id: "m1", text: "hi" }, { kind: "tool", id: "c1", name: "bash", args: "", output: "", ended: false }]), true, users);
    expect(out).toHaveLength(1);
    expect(out[0]).toMatchObject({ id: "r1-a0", role: "assistant", live: true });
    expect(out[0]?.parts.map((p) => p.kind)).toEqual(["text", "tool"]);
  });

  it("is not live once the run stopped streaming", () => {
    const out = messagesFromLive(live([{ kind: "text", id: "m1", text: "hi" }], { phase: "completed" }), false, users);
    expect(out).toHaveLength(1);
    expect(out[0]?.live).toBe(false);
  });

  it("splits the assistant message at a steer, resolving the author from the directory", () => {
    const out = messagesFromLive(
      live([
        { kind: "text", id: "m1", text: "before" },
        { kind: "steer", id: "s1", text: "look at x", delivered: true, author: "bob" },
        { kind: "text", id: "m2", text: "after" },
      ]),
      true,
      users,
    );
    expect(out.map((m) => m.role)).toEqual(["assistant", "user", "assistant"]);
    expect(out[1]).toMatchObject({ id: "r1-s1", steer: true, author: { login: "bob", name: "Bob Roe", kind: "user" }, parts: [{ text: "look at x" }] });
    expect(out[2]?.parts).toEqual([{ kind: "text", id: "m2", text: "after" }]);
  });

  it("marks an undelivered steer, keeps unknown authors by login and carries supervisor identity", () => {
    const out = messagesFromLive(
      live([
        { kind: "steer", id: "s1", text: "later", delivered: false, author: "zed" },
        { kind: "steer", id: "s2", text: "from bot", delivered: true, author: "sup", authorKind: "supervisor", onBehalfOf: "bob" },
        { kind: "steer", id: "s3", text: "anon", delivered: true },
      ]),
      false,
      users,
    );
    expect(out[0]).toMatchObject({ author: { login: "zed", kind: "user" }, parts: [{ text: "later\n(arrives with the next run)" }] });
    expect(out[1]?.author).toMatchObject({ login: "sup", kind: "supervisor", onBehalfOf: "bob" });
    expect(out[2]?.author).toBeUndefined();
  });

  it("adds a pending bubble after a trailing steer while running", () => {
    const out = messagesFromLive(live([{ kind: "text", id: "m1", text: "a" }, { kind: "steer", id: "s1", text: "x", delivered: true }]), true, users);
    expect(out.map((m) => m.role)).toEqual(["assistant", "user", "assistant"]);
    expect(out[2]).toMatchObject({ id: "r1-pending", live: true, parts: [] });
  });

  it("falls back to a stable id prefix when the run id is not known yet", () => {
    const out = messagesFromLive({ ...initialLive, phase: "running", parts: [] }, true, users);
    expect(out[0]?.id).toBe("live-pending");
  });
});
