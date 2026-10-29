// The chat transcript: persisted thread messages and the live run stream, both as ChatMessages.
import type { Author, Message } from "@/api/types";
import type { LivePart, LiveState, SubagentState } from "@/lib/agui";

export type ChatPart =
  | { kind: "text"; id: string; text: string }
  | { kind: "reasoning"; id: string; text: string }
  | { kind: "tool"; id: string; name: string; args: string; result?: string; isError?: boolean; output: string; ended: boolean; sub?: SubagentState }
  | { kind: "error"; id: string; code?: string; message: string }
  | { kind: "memory"; id: string; action: string; title: string; text: string; proposed: boolean };

export interface ChatMessage {
  id: string;
  role: "user" | "assistant";
  parts: ChatPart[];
  author?: Author;
  createdAt?: string;
  /** A mid-run message from a person to the agent. */
  steer?: boolean;
  /** True while the run still streams into this message. */
  live?: boolean;
}

/** Persisted thread messages → transcript. Assistant and tool messages of one turn merge into one ChatMessage. */
export function messagesFromThread(msgs: Message[], hideRunId?: string): ChatMessage[] {
  const out: ChatMessage[] = [];
  let turn: ChatMessage | null = null;
  for (const m of msgs) {
    if (hideRunId && m.runId === hideRunId && (m.role !== "user" || m.metadata?.steer)) continue;
    if (m.role === "user" || m.role === "system") {
      turn = null;
      out.push({ id: m.id, role: "user", parts: [{ kind: "text", id: m.id, text: m.content }], author: m.author, createdAt: m.createdAt, steer: m.metadata?.steer === true });
      continue;
    }
    if (m.role === "assistant") {
      if (!turn) {
        turn = { id: m.id, role: "assistant", parts: [], createdAt: m.createdAt };
        out.push(turn);
      }
      if (m.content) turn.parts.push({ kind: "text", id: m.id, text: m.content });
      for (const c of m.toolCalls ?? []) turn.parts.push({ kind: "tool", id: c.id, name: c.function.name, args: c.function.arguments, output: "", ended: true });
      if (m.error) turn.parts.push({ kind: "error", id: `${m.id}-err`, message: m.error });
    } else if (m.role === "tool" && turn) {
      const p = turn.parts.find((x) => x.kind === "tool" && x.id === m.toolCallId);
      if (p && p.kind === "tool") {
        p.result = m.content;
        p.isError = !!m.error;
      }
    }
  }
  return out;
}

function toChatPart(p: Exclude<LivePart, { kind: "steer" }>): ChatPart {
  return p;
}

/** The streaming run → transcript. Steers split the stream into assistant segments and user messages. */
export function messagesFromLive(live: LiveState, running: boolean, users: (login: string) => Author | undefined): ChatMessage[] {
  const out: ChatMessage[] = [];
  let seg: ChatMessage | null = null;
  const base = live.runId ?? "live";
  for (const p of live.parts) {
    if (p.kind === "steer") {
      seg = null;
      const a = p.author ? { ...(users(p.author) ?? { login: p.author }), kind: p.authorKind === "supervisor" ? ("supervisor" as const) : ("user" as const), onBehalfOf: p.onBehalfOf } : undefined;
      out.push({ id: `${base}-${p.id}`, role: "user", steer: true, parts: [{ kind: "text", id: p.id, text: p.delivered ? p.text : `${p.text}\n(arrives with the next run)` }], author: a });
      continue;
    }
    if (!seg) {
      seg = { id: `${base}-a${out.length}`, role: "assistant", parts: [], live: running };
      out.push(seg);
    }
    seg.parts.push(toChatPart(p));
  }
  if (running && (!seg || out[out.length - 1] !== seg)) out.push({ id: `${base}-pending`, role: "assistant", parts: [], live: true });
  return out;
}
