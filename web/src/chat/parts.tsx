// Renderers for message parts: markdown text, thinking, tool cards (with diffs and sub-agents).
import type { ReasoningMessagePartComponent, TextMessagePartComponent, ToolCallMessagePartComponent } from "@assistant-ui/react";
import { MarkdownTextPrimitive } from "@assistant-ui/react-markdown";
import { Link } from "@tanstack/react-router";
import { Bot, Brain, Check, ChevronRight, Loader2, TriangleAlert, Wrench, X } from "lucide-react";
import { createContext, lazy, Suspense, useContext, useState, type ReactNode } from "react";
import remarkGfm from "remark-gfm";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { editOf, parseArgs, summarizeArgs, type SubagentState } from "@/lib/agui";
import { cn } from "@/lib/utils";

const DiffBlock = lazy(() => import("./DiffBlock").then((m) => ({ default: m.DiffBlock })));

const remarkPlugins = [remarkGfm];
export const MarkdownText: TextMessagePartComponent = () => <MarkdownTextPrimitive remarkPlugins={remarkPlugins} className="md" smooth={false} />;
export const PlainText: TextMessagePartComponent = ({ text }) => <p className="whitespace-pre-wrap break-words text-sm">{text}</p>;

export const Reasoning: ReasoningMessagePartComponent = ({ text, status }) => {
  const running = status.type === "running";
  return (
    <Collapsible className="my-1 rounded-md border bg-muted/40">
      <CollapsibleTrigger className="group flex w-full items-center gap-2 px-3 py-1.5 text-xs text-muted-foreground">
        <ChevronRight className="size-3 transition-transform group-data-[state=open]:rotate-90" />
        <Brain className="size-3" />
        {running ? "Thinking…" : "Thought"}
      </CollapsibleTrigger>
      <CollapsibleContent>
        <p className="whitespace-pre-wrap px-3 pb-2 text-xs text-muted-foreground">{text}</p>
      </CollapsibleContent>
    </Collapsible>
  );
};

export interface ToolArtifact {
  output?: string;
  ended?: boolean;
  sub?: SubagentState;
}
const clip = (s: string, n = 4000) => (s.length > n ? `${s.slice(0, n)}\n… (${s.length - n} more characters)` : s);

function SubagentCard({ sub }: { sub: SubagentState }) {
  return (
    <div className="ml-2 space-y-1 border-l-2 pl-3 text-xs">
      <div className="flex items-center gap-1.5 font-medium">
        <Bot className="size-3" />
        {sub.agent || "sub-agent"}
        {sub.model ? <span className="font-normal text-muted-foreground">{sub.model}</span> : null}
        <span className="font-normal text-muted-foreground">
          {sub.done ? "done" : `step ${sub.step}`}
        </span>
        {!sub.done ? <Loader2 className="size-3 animate-spin" /> : null}
      </div>
      {sub.tools.map((t, i) => (
        <div key={i} className="flex items-center gap-1.5 text-muted-foreground">
          {t.isError ? <X className="size-3 text-destructive" /> : t.result !== undefined ? <Check className="size-3" /> : <Loader2 className="size-3 animate-spin" />}
          <code>{t.name}</code>
          {t.args && typeof t.args === "object" ? <span className="truncate">{summarizeArgs(t.args as Record<string, unknown>)}</span> : null}
        </div>
      ))}
      {sub.text ? <p className="whitespace-pre-wrap text-muted-foreground">{clip(sub.text, 800)}</p> : null}
    </div>
  );
}

function Pre({ children, className }: { children: ReactNode; className?: string }) {
  return <pre className={cn("max-h-64 overflow-auto whitespace-pre-wrap break-words rounded bg-muted p-2 text-xs", className)}>{children}</pre>;
}

export const ToolCard: ToolCallMessagePartComponent = ({ toolName, argsText, result, isError, artifact }) => {
  const art = (artifact ?? {}) as ToolArtifact;
  const args = parseArgs(argsText ?? "");
  const edit = editOf(toolName, args);
  const done = result !== undefined;
  const [open, setOpen] = useState(false);
  const summary = summarizeArgs(args);
  const res = typeof result === "string" ? result : result === undefined ? "" : JSON.stringify(result);
  return (
    <Collapsible open={open} onOpenChange={setOpen} className={cn("my-1 rounded-md border bg-card text-card-foreground", isError && "border-destructive/50")}>
      <CollapsibleTrigger className="group flex w-full items-center gap-2 px-3 py-1.5 text-left text-xs">
        <ChevronRight className="size-3 shrink-0 transition-transform group-data-[state=open]:rotate-90" />
        {isError ? <TriangleAlert className="size-3 shrink-0 text-destructive" /> : done ? <Wrench className="size-3 shrink-0 text-muted-foreground" /> : <Loader2 className="size-3 shrink-0 animate-spin" />}
        <span className="font-medium">{toolName}</span>
        <span className="truncate text-muted-foreground">{summary}</span>
      </CollapsibleTrigger>
      {art.sub ? (
        <div className="px-3 pb-2">
          <SubagentCard sub={art.sub} />
        </div>
      ) : null}
      <CollapsibleContent className="space-y-2 px-3 pb-3">
        {edit ? (
          <Suspense fallback={<Pre>Loading diff…</Pre>}>
            <DiffBlock path={edit.path} oldText={edit.oldText} newText={edit.newText} />
          </Suspense>
        ) : argsText && argsText !== "{}" ? (
          <Pre>{clip(argsText, 2000)}</Pre>
        ) : null}
        {art.output && !res ? <Pre>{clip(art.output)}</Pre> : null}
        {res && !(edit && !isError) ? <Pre className={isError ? "text-destructive" : undefined}>{clip(res)}</Pre> : null}
      </CollapsibleContent>
    </Collapsible>
  );
};

/** The project whose memory review queue a "proposed" note links to (undefined: the user's global memory). */
export const MemoryLinkContext = createContext<string | undefined>(undefined);

export function MemoryNote({ data }: { data: unknown }) {
  const d = data as { action?: string; title?: string; text?: string; proposed?: boolean };
  const project = useContext(MemoryLinkContext);
  const verb = d.action === "proposed" ? "suggested a memory" : d.action === "deleted" ? "deleted a memory" : d.action === "updated" ? "updated a memory" : "saved a memory";
  return (
    <div className="my-1 flex flex-wrap items-center gap-1.5 rounded-md border border-dashed px-3 py-1.5 text-xs text-muted-foreground">
      <Brain className="size-3" />
      <span>
        {verb}: <span className="text-foreground">{d.title}</span>
      </span>
      {d.proposed ? (
        project ? (
          <Link to="/p/$projectId" params={{ projectId: project }} search={{ tab: "memory" }} className="ml-auto text-primary underline">
            Review
          </Link>
        ) : (
          <Link to="/memory" className="ml-auto text-primary underline">
            Review
          </Link>
        )
      ) : null}
    </div>
  );
}
