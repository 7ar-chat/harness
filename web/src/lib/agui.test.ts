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

describe("work.memory", () => {
  it("records memory notes and flags proposals", () => {
    const st = run([
      { type: "CUSTOM", name: "work.memory", value: { action: "created", title: "Use tabs", text: "x" } },
      { type: "CUSTOM", name: "work.memory", value: { action: "proposed", title: "CI is slow", text: "y", proposal: "create" } },
    ]);
    expect(st.parts).toMatchObject([{ kind: "memory", action: "created", proposed: false }, { kind: "memory", action: "proposed", proposed: true, title: "CI is slow" }]);
  });
});

describe("reduceEvents edge cases", () => {
  it("does not duplicate a text part on a repeated start, and keeps interleaved messages apart", () => {
    const st = run([
      { type: "TEXT_MESSAGE_START", messageId: "m1" },
      { type: "TEXT_MESSAGE_START", messageId: "m1" },
      { type: "TEXT_MESSAGE_CONTENT", messageId: "m1", delta: "a" },
      { type: "TEXT_MESSAGE_START", messageId: "m2" },
      { type: "TEXT_MESSAGE_CONTENT", messageId: "m1", delta: "b" },
      { type: "TEXT_MESSAGE_CONTENT", messageId: "m2", delta: "c" },
    ]);
    expect(st.parts).toEqual([{ kind: "text", id: "m1", text: "ab" }, { kind: "text", id: "m2", text: "c" }]);
  });

  it("appends id-less content to the latest text part", () => {
    const st = run([{ type: "TEXT_MESSAGE_START", messageId: "m1" }, { type: "TEXT_MESSAGE_CONTENT", delta: "x" }, { type: "TEXT_MESSAGE_CONTENT", delta: "y" }]);
    expect(st.parts).toEqual([{ kind: "text", id: "m1", text: "xy" }]);
  });

  it("ignores tool events without an id, duplicate starts and events for unknown calls", () => {
    const st = run([
      { type: "TOOL_CALL_START", toolCallName: "bash" },
      { type: "TOOL_CALL_START", toolCallId: "c1", toolCallName: "bash" },
      { type: "TOOL_CALL_ARGS", toolCallId: "c1", delta: "{}" },
      { type: "TOOL_CALL_START", toolCallId: "c1", toolCallName: "bash" },
      { type: "TOOL_CALL_ARGS", toolCallId: "ghost", delta: "zzz" },
      { type: "TOOL_CALL_RESULT", toolCallId: "ghost", content: "zzz" },
      { type: "CUSTOM", name: "work.tool_output", value: { toolCallId: "ghost", text: "zzz" } },
    ]);
    expect(st.parts).toEqual([{ kind: "tool", id: "c1", name: "bash", args: "{}", output: "", ended: false }]);
  });

  it("a tool result without error metadata is not an error and marks the call ended", () => {
    const st = run([{ type: "TOOL_CALL_START", toolCallId: "c1", toolCallName: "x" }, { type: "TOOL_CALL_RESULT", toolCallId: "c1", content: "ok" }]);
    expect(st.parts[0]).toMatchObject({ ended: true, result: "ok", isError: false });
  });

  it("keeps a run id already known when RUN_STARTED carries none", () => {
    const st = run([{ type: "RUN_STARTED" }], { ...initialLive, runId: "r9" });
    expect(st).toMatchObject({ runId: "r9", phase: "running" });
  });

  it("keeps the usage reported mid-run over the one on RUN_FINISHED", () => {
    const st = run([
      { type: "CUSTOM", name: "work.usage", value: { inputTokens: 10 } },
      { type: "RUN_FINISHED", outcome: { type: "completed" }, usage: [{ inputTokens: 99 }] },
    ]);
    expect(st.usage?.inputTokens).toBe(10);
  });

  it("takes usage from RUN_FINISHED when none was reported and ignores an empty usage event", () => {
    const st = run([{ type: "CUSTOM", name: "work.usage", value: {} }, { type: "RUN_FINISHED", outcome: { type: "completed" }, usage: [{ inputTokens: 99 }] }]);
    expect(st.usage?.inputTokens).toBe(99);
  });

  it("treats an interrupt outcome with no interrupts as completed", () => {
    expect(run([{ type: "RUN_FINISHED", outcome: { type: "interrupt", interrupts: [] } }]).phase).toBe("completed");
  });

  it("leaves the plan alone for a snapshot without one, and replaces it for a new one", () => {
    const withPlan = run([{ type: "STATE_SNAPSHOT", snapshot: { plan: [{ id: "1", title: "a", status: "pending" }] } }]);
    expect(run([{ type: "STATE_SNAPSHOT", snapshot: { other: 1 } }], withPlan).plan).toEqual(withPlan.plan);
    expect(run([{ type: "STATE_SNAPSHOT", snapshot: { plan: [] } }], withPlan).plan).toEqual([]);
  });

  it("ignores an empty title and keeps the old one", () => {
    const st = run([{ type: "CUSTOM", name: "work.title", value: { title: "A" } }, { type: "CUSTOM", name: "work.title", value: { title: "" } }]);
    expect(st.title).toBe("A");
  });

  it("flags a steer as undelivered only when delivered is explicitly false", () => {
    const st = run([
      { type: "CUSTOM", name: "work.steer", value: { id: "a", text: "1", delivered: false } },
      { type: "CUSTOM", name: "work.steer", value: { text: "2" } },
    ]);
    expect(st.parts).toMatchObject([{ id: "a", delivered: false }, { id: "steer-1", delivered: true }]);
  });

  it("a memory event with proposed:true is a proposal even when the action is something else", () => {
    const st = run([{ type: "CUSTOM", name: "work.memory", value: { action: "updated", title: "t", text: "x", proposed: true } }]);
    expect(st.parts[0]).toMatchObject({ kind: "memory", proposed: true });
  });

  it("a RUN_ERROR without a message still explains itself and appends after streamed text", () => {
    const st = run([{ type: "TEXT_MESSAGE_CONTENT", messageId: "m", delta: "a" }, { type: "RUN_ERROR" }]);
    expect(st.parts[1]).toMatchObject({ kind: "error", message: "The run failed.", code: undefined });
  });

  it("an untouched sub-agent tool pairs a result with the earlier call of the same name", () => {
    const sub = (value: Record<string, unknown>): AguiEvent => ({ type: "CUSTOM", name: "work.subagent", value: { toolCallId: "c1", ...value } });
    const st = run([
      { type: "TOOL_CALL_START", toolCallId: "c1", toolCallName: "task" },
      sub({ phase: "tool", tool: { name: "read", args: { path: "a" } } }),
      sub({ phase: "tool", tool: { name: "grep", args: {} } }),
      sub({ phase: "tool", tool: { name: "read", result: "body", isError: true } }),
      sub({ phase: "step" }),
      sub({ phase: "step" }),
    ]);
    const t = st.parts[0];
    const s = t && t.kind === "tool" ? t.sub : undefined;
    expect(s?.tools).toEqual([{ name: "read", args: undefined, result: "body", isError: true }, { name: "grep", args: {}, result: undefined, isError: false }]);
    expect(s?.step).toBe(2);
  });
});

describe("parseArgs and summarizeArgs", () => {
  it("returns an empty object for partial JSON, arrays, scalars and null", () => {
    for (const bad of ['{"a":', "[1]", "3", "null", ""]) expect(parseArgs(bad)).toEqual({});
  });
  it("summarises with the first known key, collapsing whitespace and clipping to 120 chars", () => {
    expect(summarizeArgs({ command: "ls   -la\n/tmp", path: "ignored?" })).toBe("ignored?");
    expect(summarizeArgs({ command: "ls   -la\n/tmp" })).toBe("ls -la /tmp");
    expect(summarizeArgs({ query: "x".repeat(300) })).toHaveLength(120);
  });
  it("skips empty and non-string values and returns an empty string when nothing matches", () => {
    expect(summarizeArgs({ path: "", command: 5, url: "u" })).toBe("u");
    expect(summarizeArgs({ other: "x" })).toBe("");
  });
  it("editOf needs both texts and tolerates a missing path", () => {
    expect(editOf("edit_file", { oldText: "a" })).toBeNull();
    expect(editOf("edit_file", { oldText: "a", newText: "" })).toEqual({ path: "", oldText: "a", newText: "" });
  });
});
