// The cards for an interrupted run: tool approval and ask_user question.
import { Check, MessageCircleQuestion, ShieldAlert, X } from "lucide-react";
import { useState } from "react";
import type { Interrupt } from "@/lib/agui";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";

type ResumeFn = (b: { interruptId: string; reason: string; decision?: "approve" | "deny"; always?: boolean; text?: string }) => Promise<void>;

export function InterruptCard({ interrupt, resume, busy, disabled, askedUser }: { interrupt: Interrupt; resume: ResumeFn; busy: boolean; disabled: boolean; askedUser?: string }) {
  return interrupt.reason === "tool_approval" ? (
    <ApprovalCard interrupt={interrupt} resume={resume} busy={busy} disabled={disabled} askedUser={askedUser} />
  ) : (
    <QuestionCard interrupt={interrupt} resume={resume} busy={busy} disabled={disabled} askedUser={askedUser} />
  );
}

type Props = { interrupt: Interrupt; resume: ResumeFn; busy: boolean; disabled: boolean; askedUser?: string };

export function ApprovalCard({ interrupt, resume, busy, disabled, askedUser }: Props) {
  const a = interrupt.approval ?? {};
  const id = a.id || interrupt.id;
  const act = (decision: "approve" | "deny", always = false) => resume({ interruptId: id, reason: "tool_approval", decision, always });
  return (
    <div role="alertdialog" aria-label="Approval needed" className="rounded-lg border border-warning/50 bg-warning/10 p-3">
      <div className="flex items-center gap-2 text-sm font-medium">
        <ShieldAlert className="size-4 text-warning" />
        Approval needed: <code>{a.toolName || interrupt.message || "tool"}</code>
      </div>
      {askedUser ? <div className="mt-0.5 text-xs text-muted-foreground">Asking {askedUser}</div> : null}
      {a.toolInputPreview ? <pre className="mt-2 max-h-40 overflow-auto whitespace-pre-wrap break-words rounded bg-background/60 p-2 text-xs">{a.toolInputPreview}</pre> : null}
      <div className="mt-3 flex flex-wrap gap-2">
        <Button size="sm" disabled={busy || disabled} onClick={() => void act("approve")}>
          <Check /> Approve
        </Button>
        <Button size="sm" variant="secondary" disabled={busy || disabled} onClick={() => void act("approve", true)}>
          Approve always
        </Button>
        <Button size="sm" variant="destructive" disabled={busy || disabled} onClick={() => void act("deny")}>
          <X /> Deny
        </Button>
      </div>
    </div>
  );
}

export function QuestionCard({ interrupt, resume, busy, disabled, askedUser }: Props) {
  const [text, setText] = useState("");
  const answer = (t: string) => resume({ interruptId: interrupt.id, reason: interrupt.reason, text: t });
  const schema = interrupt.responseSchema ?? {};
  return (
    <div role="alertdialog" aria-label="Question from the agent" className="rounded-lg border border-primary/30 bg-primary/5 p-3">
      <div className="flex items-center gap-2 text-sm font-medium">
        <MessageCircleQuestion className="size-4" />
        {interrupt.message || "The agent has a question"}
      </div>
      {askedUser ? <div className="mt-0.5 text-xs text-muted-foreground">Asking {askedUser}</div> : null}
      {schema.options?.length ? (
        <div className="mt-2 flex flex-wrap gap-2">
          {schema.options.map((o) => (
            <Button key={o.label} size="sm" variant="outline" title={o.description} disabled={busy || disabled} onClick={() => void answer(o.label)}>
              {o.label}
            </Button>
          ))}
        </div>
      ) : null}
      {schema.allowFreeText !== false ? (
        <form
          className="mt-2 flex gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            if (text.trim()) void answer(text.trim());
          }}
        >
          <Input value={text} onChange={(e) => setText(e.target.value)} placeholder="Your answer" aria-label="Your answer" disabled={disabled} />
          <Button size="sm" type="submit" disabled={busy || disabled || !text.trim()}>
            Answer
          </Button>
        </form>
      ) : null}
    </div>
  );
}
