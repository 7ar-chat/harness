// The session view's Diff tab: the file edits the agent made (its edit_file calls), one diff per edit.
import { lazy, Suspense } from "react";
import type { Message } from "@/api/types";
import { Empty } from "@/components/app/common";
import { editOf, parseArgs } from "@/lib/agui";

const DiffBlock = lazy(() => import("@/chat/DiffBlock").then((m) => ({ default: m.DiffBlock })));

export function sessionEdits(messages: Message[]) {
  return messages.flatMap((m) =>
    (m.toolCalls ?? []).flatMap((c) => {
      const e = editOf(c.function.name, parseArgs(c.function.arguments ?? ""));
      return e ? [e] : [];
    }),
  );
}

export function DiffPanel({ messages }: { messages: Message[] }) {
  const edits = sessionEdits(messages);
  if (!edits.length) return <Empty title="No edits yet">Changes the agent makes to files show up here as diffs.</Empty>;
  return (
    <div className="space-y-4 p-3">
      {edits.map((e, i) => (
        <section key={i} aria-label={`Edit of ${e.path}`} className="space-y-1">
          <div className="truncate font-mono text-xs text-muted-foreground">{e.path}</div>
          <Suspense fallback={<p className="text-xs text-muted-foreground">Loading diff…</p>}>
            <DiffBlock path={e.path} oldText={e.oldText} newText={e.newText} />
          </Suspense>
        </section>
      ))}
    </div>
  );
}
