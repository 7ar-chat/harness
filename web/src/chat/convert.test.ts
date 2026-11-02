import { describe, expect, it } from "vitest";
import { toThreadMessage } from "./convert";
import type { ChatMessage } from "./model";

describe("toThreadMessage", () => {
  it("keeps only the text of a user message and carries author, steer and time for the renderers", () => {
    const m: ChatMessage = { id: "u1", role: "user", parts: [{ kind: "text", id: "p", text: "hello" }], author: { login: "alice" }, steer: true, createdAt: "t" };
    expect(toThreadMessage(m)).toEqual({ id: "u1", role: "user", content: [{ type: "text", text: "hello" }], metadata: { custom: { author: { login: "alice" }, steer: true, createdAt: "t" } } });
  });

  it("maps assistant parts: reasoning, memory notes and errors as text", () => {
    const m: ChatMessage = {
      id: "a1",
      role: "assistant",
      parts: [
        { kind: "reasoning", id: "r", text: "hm" },
        { kind: "text", id: "t", text: "answer" },
        { kind: "memory", id: "m", action: "proposed", title: "T", text: "X", proposed: true },
        { kind: "error", id: "e", code: "model_error", message: "boom" },
        { kind: "error", id: "e2", message: "plain" },
      ],
    };
    const out = toThreadMessage(m);
    expect(out.content).toEqual([
      { type: "reasoning", text: "hm" },
      { type: "text", text: "answer" },
      { type: "data-memory", id: "m", data: { action: "proposed", title: "T", text: "X", proposed: true } },
      { type: "text", text: "**Error (model_error):** boom" },
      { type: "text", text: "**Error:** plain" },
    ]);
  });

  it("is running while live and complete otherwise", () => {
    const base: ChatMessage = { id: "a", role: "assistant", parts: [] };
    expect(toThreadMessage({ ...base, live: true })).toMatchObject({ status: { type: "running" } });
    expect(toThreadMessage(base)).toMatchObject({ status: { type: "complete", reason: "stop" } });
  });

  it("parses tool arguments leniently while still passing the raw text and artifact", () => {
    const sub = { step: 1, text: "", tools: [], done: false };
    const m: ChatMessage = {
      id: "a",
      role: "assistant",
      parts: [
        { kind: "tool", id: "c1", name: "bash", args: '{"command":"ls"}', output: "out", ended: false, sub },
        { kind: "tool", id: "c2", name: "bash", args: '{"comm', output: "", ended: false, result: "r", isError: true },
      ],
    };
    const [a, b] = toThreadMessage(m).content as unknown as { toolCallId: string; args: unknown; argsText: string; result?: string; isError?: boolean; artifact: unknown }[];
    expect(a).toMatchObject({ toolCallId: "c1", args: { command: "ls" }, argsText: '{"command":"ls"}', artifact: { output: "out", ended: false, sub } });
    expect(b).toMatchObject({ toolCallId: "c2", args: {}, argsText: '{"comm', result: "r", isError: true });
  });
});
