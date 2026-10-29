// Pure reducer turning the AG-UI event stream of one run into renderable parts.
// Ported from the old vanilla client (hosts/web/ui/app.js) and extended with
// reasoning, sub-agents, steers, plan snapshots and interrupts.

export type AguiEvent = { type: string } & Record<string, unknown>;

export interface Usage {
  model?: string;
  inputTokens?: number;
  outputTokens?: number;
  totalTokens?: number;
  reasoningTokens?: number;
  cachedInputTokens?: number;
  contextTokens?: number;
  contextWindow?: number;
  costUsd?: number;
}

export interface SubagentTool {
  name: string;
  args?: unknown;
  result?: string;
  isError?: boolean;
}
export interface SubagentState {
  agent?: string;
  model?: string;
  step: number;
  text: string;
  tools: SubagentTool[];
  done: boolean;
}

export type LivePart =
  | { kind: "text"; id: string; text: string }
  | { kind: "reasoning"; id: string; text: string }
  | {
      kind: "tool";
      id: string;
      name: string;
      args: string;
      result?: string;
      isError?: boolean;
      output: string;
      ended: boolean;
      sub?: SubagentState;
    }
  | { kind: "steer"; id: string; text: string; delivered: boolean; author?: string; authorKind?: string; onBehalfOf?: string }
  | { kind: "error"; id: string; code?: string; message: string };

export interface Interrupt {
  id: string;
  reason: "tool_approval" | "ask_user" | string;
  message?: string;
  toolCallId?: string;
  approval?: { id?: string; toolName?: string; toolInputPreview?: string };
  responseSchema?: { options?: { label: string; description?: string }[]; allowFreeText?: boolean };
}

export type RunPhase = "idle" | "running" | "completed" | "cancelled" | "interrupted" | "failed";

export interface PlanEntry {
  id: string;
  title: string;
  status: string;
}

export interface LiveState {
  runId?: string;
  phase: RunPhase;
  parts: LivePart[];
  steps: number;
  usage?: Usage;
  plan?: PlanEntry[];
  interrupt?: Interrupt;
  title?: string;
  /** Highest SSE seq applied (set by the caller). */
  seq: number;
}

export const initialLive: LiveState = { phase: "idle", parts: [], steps: 0, seq: 0 };

const s = (v: unknown): string => (typeof v === "string" ? v : "");
const rec = (v: unknown): Record<string, unknown> => (v && typeof v === "object" ? (v as Record<string, unknown>) : {});

function updateTool(parts: LivePart[], id: string, f: (t: Extract<LivePart, { kind: "tool" }>) => LivePart): LivePart[] {
  return parts.map((p) => (p.kind === "tool" && p.id === id ? f(p) : p));
}

/** The last streaming text/reasoning part with this message id, appended to or created. */
function appendTo(parts: LivePart[], kind: "text" | "reasoning", id: string, delta: string): LivePart[] {
  const i = parts.findIndex((p) => p.kind === kind && p.id === id);
  if (i < 0) return [...parts, { kind, id, text: delta }];
  return parts.map((p, j) => (j === i && (p.kind === "text" || p.kind === "reasoning") ? { ...p, text: p.text + delta } : p));
}

function usageOf(v: unknown): Usage | undefined {
  const r = rec(v);
  return Object.keys(r).length ? (r as Usage) : undefined;
}

export function reduceEvents(state: LiveState, ev: AguiEvent): LiveState {
  switch (ev.type) {
    case "RUN_STARTED":
      return { ...state, runId: s(ev.runId) || state.runId, phase: "running" };
    case "STEP_STARTED":
      return { ...state, steps: state.steps + 1 };
    case "TEXT_MESSAGE_START": {
      const id = s(ev.messageId) || `text-${state.parts.length}`;
      return { ...state, parts: state.parts.some((p) => p.kind === "text" && p.id === id) ? state.parts : [...state.parts, { kind: "text", id, text: "" }] };
    }
    case "TEXT_MESSAGE_CONTENT": {
      const id = s(ev.messageId) || lastId(state.parts, "text") || "text-0";
      return { ...state, parts: appendTo(state.parts, "text", id, s(ev.delta)) };
    }
    case "REASONING_MESSAGE_START": {
      const id = s(ev.messageId) || `reasoning-${state.parts.length}`;
      return { ...state, parts: [...state.parts, { kind: "reasoning", id, text: "" }] };
    }
    case "REASONING_MESSAGE_CONTENT": {
      const id = s(ev.messageId) || lastId(state.parts, "reasoning") || "reasoning-0";
      return { ...state, parts: appendTo(state.parts, "reasoning", id, s(ev.delta)) };
    }
    case "TOOL_CALL_START": {
      const id = s(ev.toolCallId);
      if (!id || state.parts.some((p) => p.kind === "tool" && p.id === id)) return state;
      return { ...state, parts: [...state.parts, { kind: "tool", id, name: s(ev.toolCallName), args: "", output: "", ended: false }] };
    }
    case "TOOL_CALL_ARGS":
      return { ...state, parts: updateTool(state.parts, s(ev.toolCallId), (t) => ({ ...t, args: t.args + s(ev.delta) })) };
    case "TOOL_CALL_END":
      return { ...state, parts: updateTool(state.parts, s(ev.toolCallId), (t) => ({ ...t, ended: true })) };
    case "TOOL_CALL_RESULT": {
      const isError = rec(ev.metadata).isError === true;
      return { ...state, parts: updateTool(state.parts, s(ev.toolCallId), (t) => ({ ...t, ended: true, result: s(ev.content), isError })) };
    }
    case "STATE_SNAPSHOT": {
      const plan = rec(rec(ev.snapshot)).plan;
      return Array.isArray(plan) ? { ...state, plan: plan.map((x) => ({ id: s(rec(x).id), title: s(rec(x).title), status: s(rec(x).status) })) } : state;
    }
    case "CUSTOM":
      return reduceCustom(state, s(ev.name), rec(ev.value));
    case "RUN_ERROR":
      return {
        ...state,
        phase: "failed",
        parts: [...state.parts, { kind: "error", id: `err-${state.parts.length}`, code: s(ev.code) || undefined, message: s(ev.message) || "The run failed." }],
      };
    case "RUN_FINISHED": {
      const o = rec(ev.outcome);
      const usage = Array.isArray(ev.usage) ? usageOf(ev.usage[0]) : undefined;
      const next: LiveState = { ...state, usage: state.usage ?? usage };
      if (o.type === "interrupt" && Array.isArray(o.interrupts) && o.interrupts.length) {
        return { ...next, phase: "interrupted", interrupt: normalizeInterrupt(rec(o.interrupts[0])) };
      }
      return { ...next, phase: o.type === "cancelled" ? "cancelled" : "completed" };
    }
    default:
      return state;
  }
}

function lastId(parts: LivePart[], kind: "text" | "reasoning"): string | undefined {
  for (let i = parts.length - 1; i >= 0; i--) {
    const p = parts[i];
    if (p && p.kind === kind) return p.id;
  }
  return undefined;
}

function normalizeInterrupt(r: Record<string, unknown>): Interrupt {
  const a = rec(r.approval);
  const schema = rec(r.responseSchema);
  return {
    id: s(r.id),
    reason: s(r.reason),
    message: s(r.message) || undefined,
    toolCallId: s(r.toolCallId) || undefined,
    approval: Object.keys(a).length ? { id: s(a.id) || undefined, toolName: s(a.toolName) || undefined, toolInputPreview: s(a.toolInputPreview) || undefined } : undefined,
    responseSchema: Object.keys(schema).length
      ? {
          options: Array.isArray(schema.options) ? schema.options.map((o) => ({ label: s(rec(o).label), description: s(rec(o).description) || undefined })) : undefined,
          allowFreeText: typeof schema.allowFreeText === "boolean" ? schema.allowFreeText : undefined,
        }
      : undefined,
  };
}

function reduceCustom(state: LiveState, name: string, v: Record<string, unknown>): LiveState {
  switch (name) {
    case "work.usage":
      return { ...state, usage: usageOf(v) ?? state.usage };
    case "work.title":
      return s(v.title) ? { ...state, title: s(v.title) } : state;
    case "work.tool_output":
      return { ...state, parts: updateTool(state.parts, s(v.toolCallId), (t) => ({ ...t, output: t.output + s(v.text) })) };
    case "work.steer":
      return {
        ...state,
        parts: [
          ...state.parts,
          {
            kind: "steer",
            id: s(v.id) || `steer-${state.parts.length}`,
            text: s(v.text),
            delivered: v.delivered !== false,
            author: s(v.author) || undefined,
            authorKind: s(v.authorKind) || undefined,
            onBehalfOf: s(v.onBehalfOf) || undefined,
          },
        ],
      };
    case "work.subagent": {
      const id = s(v.toolCallId);
      const phase = s(v.phase);
      return {
        ...state,
        parts: updateTool(state.parts, id, (t) => {
          const sub: SubagentState = t.sub ?? { step: 0, text: "", tools: [], done: false };
          const next = { ...sub, tools: [...sub.tools] };
          if (s(v.agent)) next.agent = s(v.agent);
          if (s(v.model)) next.model = s(v.model);
          if (phase === "step") next.step = typeof v.step === "number" ? v.step : next.step + 1;
          else if (phase === "tool") {
            const tool = rec(v.tool);
            const idx = next.tools.findIndex((x) => x.name === s(tool.name) && x.result === undefined && tool.result !== undefined);
            const entry: SubagentTool = { name: s(tool.name), args: tool.args, result: tool.result === undefined ? undefined : s(tool.result), isError: tool.isError === true };
            if (idx >= 0) next.tools[idx] = entry;
            else next.tools.push(entry);
          } else if (phase === "text") next.text += s(v.text);
          else if (phase === "end") next.done = true;
          return { ...t, sub: next };
        }),
      };
    }
    default:
      return state;
  }
}

/** Parses tool arguments leniently (they stream in as partial JSON). */
export function parseArgs(args: string): Record<string, unknown> {
  try {
    const v: unknown = JSON.parse(args);
    return v && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : {};
  } catch {
    return {};
  }
}

/** One-line summary of a tool call's input. */
export function summarizeArgs(args: Record<string, unknown>): string {
  for (const k of ["path", "command", "cmd", "pattern", "query", "url", "name", "question"]) {
    const v = args[k];
    if (typeof v === "string" && v) return v.replace(/\s+/g, " ").slice(0, 120);
  }
  return "";
}

export interface EditDiff {
  path: string;
  oldText: string;
  newText: string;
}
/** The file edit a tool call makes, when it is one. */
export function editOf(name: string, args: Record<string, unknown>): EditDiff | null {
  if (name !== "edit_file") return null;
  if (typeof args.oldText !== "string" || typeof args.newText !== "string") return null;
  return { path: s(args.path), oldText: args.oldText, newText: args.newText };
}
