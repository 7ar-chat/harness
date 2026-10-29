import type { ThreadMessageLike } from "@assistant-ui/react";
import type { ReadonlyJSONObject } from "assistant-stream/utils";
import { parseArgs } from "@/lib/agui";
import type { ChatMessage } from "./model";
import type { ToolArtifact } from "./parts";

/** What the per-message renderers read back from `message.metadata.custom`. */
export interface MessageCustom {
  author?: ChatMessage["author"];
  steer?: boolean;
  createdAt?: string;
}

export function toThreadMessage(m: ChatMessage): ThreadMessageLike {
  const custom: MessageCustom = { author: m.author, steer: m.steer, createdAt: m.createdAt };
  if (m.role === "user") {
    return { id: m.id, role: "user", content: m.parts.flatMap((p) => (p.kind === "text" ? [{ type: "text" as const, text: p.text }] : [])), metadata: { custom: { ...custom } } };
  }
  return {
    id: m.id,
    role: "assistant",
    status: m.live ? { type: "running" } : { type: "complete", reason: "stop" },
    content: m.parts.map((p) => {
      switch (p.kind) {
        case "text":
          return { type: "text" as const, text: p.text };
        case "reasoning":
          return { type: "reasoning" as const, text: p.text };
        case "error":
          return { type: "text" as const, text: `**Error${p.code ? ` (${p.code})` : ""}:** ${p.message}` };
        case "tool": {
          const artifact: ToolArtifact = { output: p.output, ended: p.ended, sub: p.sub };
          return {
            type: "tool-call" as const,
            toolCallId: p.id,
            toolName: p.name,
            args: parseArgs(p.args) as ReadonlyJSONObject,
            argsText: p.args,
            result: p.result,
            isError: p.isError,
            artifact,
          };
        }
      }
    }),
    metadata: { custom: { ...custom } },
  };
}
