import { describe, expect, it } from "vitest";
import { editOf, initialLive, parseArgs, reduceEvents, summarizeArgs, type AguiEvent, type LiveState } from "./agui";

const run = (events: AguiEvent[], from: LiveState = initialLive) => events.reduce(reduceEvents, from);

describe("reduceEvents", () => {
  it("streams assistant text", () => {
    const st = run([
      { type: "RUN_STARTED", runId: "r1" },
      { type: "STEP_STARTED", stepName: "model" },
      { type: "TEXT_MESSAGE_START", messageId: "m1", role: "assistant" },
      { type: "TEXT_MESSAGE_CONTENT", messageId: "m1", delta: "Hel" },
      { type: "TEXT_MESSAGE_CONTENT", messageId: "m1", delta: "lo" },
      { type: "TEXT_MESSAGE_END", messageId: "m1" },
    ]);
    expect(st.phase).toBe("running");
    expect(st.steps).toBe(1);
    expect(st.parts).toEqual([{ kind: "text", id: "m1", text: "Hello" }]);
  });

  it("accepts content without a start event", () => {
    const st = run([{ type: "TEXT_MESSAGE_CONTENT", delta: "hi" }]);
    expect(st.parts[0]).toMatchObject({ kind: "text", text: "hi" });
  });

  it("builds reasoning parts", () => {
    const st = run([
      { type: "REASONING_START", messageId: "t" },
      { type: "REASONING_MESSAGE_START", messageId: "t" },
      { type: "REASONING_MESSAGE_CONTENT", messageId: "t", delta: "think" },
      { type: "REASONING_MESSAGE_CONTENT", messageId: "t", delta: "ing" },
    ]);
    expect(st.parts).toEqual([{ kind: "reasoning", id: "t", text: "thinking" }]);
  });

  it("tracks a tool call with streamed args, output and result", () => {
    const st = run([
      { type: "TOOL_CALL_START", toolCallId: "c1", toolCallName: "bash" },
      { type: "TOOL_CALL_ARGS", toolCallId: "c1", delta: '{"comm' },
      { type: "TOOL_CALL_ARGS", toolCallId: "c1", delta: 'and":"ls"}' },
      { type: "TOOL_CALL_END", toolCallId: "c1" },
      { type: "CUSTOM", name: "work.tool_output", value: { toolCallId: "c1", text: "a\n" } },
      { type: "CUSTOM", name: "work.tool_output", value: { toolCallId: "c1", text: "b\n" } },
      { type: "TOOL_CALL_RESULT", toolCallId: "c1", content: "a\nb\n", metadata: { isError: false } },
    ]);
    const t = st.parts[0];
    expect(t).toMatchObject({ kind: "tool", name: "bash", output: "a\nb\n", result: "a\nb\n", ended: true, isError: false });
    expect(summarizeArgs(parseArgs(t && t.kind === "tool" ? t.args : ""))).toBe("ls");
  });

  it("flags tool errors", () => {
    const st = run([
      { type: "TOOL_CALL_START", toolCallId: "c1", toolCallName: "read_file" },
      { type: "TOOL_CALL_RESULT", toolCallId: "c1", content: "nope", metadata: { isError: true } },
    ]);
    expect(st.parts[0]).toMatchObject({ isError: true });
  });

  it("extracts an edit diff", () => {
    const args = parseArgs('{"path":"a.go","oldText":"x","newText":"y"}');
    expect(editOf("edit_file", args)).toEqual({ path: "a.go", oldText: "x", newText: "y" });
    expect(editOf("bash", args)).toBeNull();
  });

  it("applies usage, title, plan and steer events", () => {
    const st = run([
      { type: "CUSTOM", name: "work.usage", value: { inputTokens: 1200, outputTokens: 300 } },
      { type: "CUSTOM", name: "work.title", value: { title: "Fix it" } },
      { type: "STATE_SNAPSHOT", snapshot: { plan: [{ id: "1", title: "a", status: "pending" }] } },
      { type: "CUSTOM", name: "work.steer", value: { id: "s1", text: "also x", delivered: true, author: "bob", authorKind: "supervisor", onBehalfOf: "alice" } },
    ]);
    expect(st.usage?.inputTokens).toBe(1200);
    expect(st.title).toBe("Fix it");
    expect(st.plan).toEqual([{ id: "1", title: "a", status: "pending" }]);
    expect(st.parts[0]).toMatchObject({ kind: "steer", author: "bob", authorKind: "supervisor", onBehalfOf: "alice", delivered: true });
  });

  it("nests sub-agent progress under its tool call", () => {
    const sub = (value: Record<string, unknown>): AguiEvent => ({ type: "CUSTOM", name: "work.subagent", value: { toolCallId: "c1", ...value } });
    const st = run([
      { type: "TOOL_CALL_START", toolCallId: "c1", toolCallName: "task" },
      sub({ phase: "start", agent: "explore", model: "m" }),
      sub({ phase: "step", step: 1 }),
      sub({ phase: "tool", tool: { name: "grep", args: { pattern: "x" } } }),
      sub({ phase: "tool", tool: { name: "grep", result: "hit" } }),
      sub({ phase: "text", text: "found" }),
      sub({ phase: "end" }),
    ]);
    const t = st.parts[0];
    expect(t && t.kind === "tool" ? t.sub : null).toMatchObject({ agent: "explore", step: 1, text: "found", done: true, tools: [{ name: "grep", result: "hit" }] });
  });

  it("ends in an interrupt for a tool approval", () => {
    const st = run([
      { type: "RUN_STARTED", runId: "r1" },
      {
        type: "RUN_FINISHED",
        outcome: {
          type: "interrupt",
          interrupts: [{ id: "a1", reason: "tool_approval", message: "write_file", toolCallId: "w1", approval: { id: "a1", toolName: "write_file", toolInputPreview: "path=/x" } }],
        },
        usage: [{ inputTokens: 5 }],
      },
    ]);
    expect(st.phase).toBe("interrupted");
    expect(st.interrupt).toMatchObject({ id: "a1", reason: "tool_approval", approval: { toolName: "write_file", toolInputPreview: "path=/x" } });
    expect(st.usage?.inputTokens).toBe(5);
  });

  it("ends in an interrupt for an ask_user question", () => {
    const st = run([
      {
        type: "RUN_FINISHED",
        outcome: { type: "interrupt", interrupts: [{ id: "q1", reason: "ask_user", message: "Which?", responseSchema: { options: [{ label: "A" }], allowFreeText: false } }] },
      },
    ]);
    expect(st.interrupt).toMatchObject({ reason: "ask_user", message: "Which?", responseSchema: { options: [{ label: "A" }], allowFreeText: false } });
  });

  it("handles completed, cancelled and error endings", () => {
    expect(run([{ type: "RUN_FINISHED", outcome: { type: "completed" } }]).phase).toBe("completed");
    expect(run([{ type: "RUN_FINISHED", outcome: { type: "cancelled" } }]).phase).toBe("cancelled");
    const failed = run([{ type: "RUN_ERROR", code: "model_error", message: "boom" }]);
    expect(failed.phase).toBe("failed");
    expect(failed.parts[0]).toMatchObject({ kind: "error", code: "model_error", message: "boom" });
  });

  it("ignores unknown events and does not mutate its input", () => {
    const before = run([{ type: "TEXT_MESSAGE_CONTENT", messageId: "m", delta: "a" }]);
    const snapshot = JSON.stringify(before);
    const after = reduceEvents(before, { type: "TEXT_MESSAGE_CONTENT", messageId: "m", delta: "b" });
    expect(JSON.stringify(before)).toBe(snapshot);
    expect(reduceEvents(after, { type: "SOMETHING_NEW" })).toBe(after);
  });
});
