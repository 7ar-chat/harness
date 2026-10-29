// The chat window: assistant-ui renders messages, composer and autoscroll; the transcript comes from useChat.
import {
  AssistantRuntimeProvider, AttachmentPrimitive, ComposerPrimitive, MessagePrimitive, SimpleImageAttachmentAdapter, ThreadPrimitive, useAui, useAuiState, useExternalStoreRuntime,
  type AppendMessage,
} from "@assistant-ui/react";
import { Bot, Paperclip, Send, X } from "lucide-react";
import { useCallback, useEffect, useMemo, type ReactNode } from "react";
import { toast } from "sonner";
import { api } from "@/api/client";
import { useCommands } from "@/api/queries";
import type { Attachment, Author, Command } from "@/api/types";
import { UserAvatar } from "@/components/app/common";
import { Button } from "@/components/ui/button";
import { markSeen } from "@/lib/unread";
import { toThreadMessage, type MessageCustom } from "./convert";
import { MarkdownText, MemoryLinkContext, MemoryNote, PlainText, Reasoning, ToolCard } from "./parts";
import { InterruptCard } from "./Prompts";
import { useChat, type RunSettings } from "./useChat";
import { formatUsage } from "./usage";
import { format } from "date-fns";
import type { ModelInfo } from "@/api/types";
import type { ChatMessage } from "./model";

const adapter = new SimpleImageAttachmentAdapter();

function AuthorHeader({ author, steer, at }: { author?: Author; steer?: boolean; at?: string }) {
  const name = author?.name || author?.login || (author?.kind === "system" ? "Watcher" : author?.kind === "supervisor" ? "My assistant" : "You");
  return (
    <div className="mb-1 flex flex-wrap items-center gap-x-2 text-xs text-muted-foreground">
      {author?.login ? <UserAvatar login={author.login} name={author.name} url={author.avatarUrl} className="size-5" /> : null}
      <span className="font-medium text-foreground">{name}</span>
      {author?.kind === "supervisor" ? <span className="rounded bg-secondary px-1.5 py-0.5">{author.login ? "via my assistant" : "my assistant"}{author.onBehalfOf ? ` for ${author.onBehalfOf}` : ""}</span> : null}
      {steer ? <span className="rounded bg-secondary px-1.5 py-0.5">steer</span> : null}
      {at ? <time dateTime={at}>{format(new Date(at), "HH:mm")}</time> : null}
    </div>
  );
}

function UserMessage() {
  const custom = useAuiState((s) => s.message.metadata.custom) as MessageCustom | undefined;
  return (
    <MessagePrimitive.Root className="flex justify-end px-4 py-2">
      <div className="max-w-[85%] min-w-0">
        <AuthorHeader author={custom?.author} steer={custom?.steer} at={custom?.createdAt} />
        <div className="rounded-2xl rounded-tr-sm bg-primary/10 px-3 py-2">
          <MessagePrimitive.Parts components={{ Text: PlainText }} />
        </div>
      </div>
    </MessagePrimitive.Root>
  );
}

function Thinking() {
  const running = useAuiState((s) => s.message.status?.type === "running");
  return running ? <span className="text-xs text-muted-foreground">Working…</span> : null;
}

function AssistantMessage() {
  return (
    <MessagePrimitive.Root className="px-4 py-2">
      <div className="mb-1 flex items-center gap-2 text-xs text-muted-foreground">
        <span className="flex size-5 items-center justify-center rounded-full bg-secondary">
          <Bot className="size-3" />
        </span>
        <span className="font-medium text-foreground">Agent</span>
      </div>
      <div className="min-w-0 pl-7">
        <MessagePrimitive.Parts components={{ Text: MarkdownText, Reasoning, tools: { Fallback: ToolCard }, Empty: Thinking, data: { by_name: { memory: ({ data }) => <MemoryNote data={data} /> } } }} />
      </div>
    </MessagePrimitive.Root>
  );
}

function SlashMenu({ commands }: { commands: Command[] }) {
  const aui = useAui();
  const text = useAuiState((s) => s.composer.text);
  if (!text.startsWith("/") || text.includes(" ")) return null;
  const q = text.slice(1).toLowerCase();
  const list = commands.filter((c) => c.name.toLowerCase().startsWith(q)).slice(0, 6);
  if (!list.length) return null;
  return (
    <ul className="mb-2 overflow-hidden rounded-md border bg-popover text-sm shadow-sm" role="listbox" aria-label="Commands">
      {list.map((c) => (
        <li key={c.name}>
          <button type="button" role="option" aria-selected={false} className="flex w-full items-baseline gap-2 px-3 py-1.5 text-left hover:bg-accent" onClick={() => aui.composer().setText(`/${c.name} `)}>
            <code>/{c.name}</code>
            <span className="truncate text-xs text-muted-foreground">{c.argHint ? `${c.argHint} ` : ""}{c.description}</span>
          </button>
        </li>
      ))}
    </ul>
  );
}

function Composer({ running, readOnly, commands }: { running: boolean; readOnly: boolean; commands: Command[] }) {
  return (
    <ComposerPrimitive.Root className="rounded-xl border bg-card p-2 shadow-sm focus-within:ring-2 focus-within:ring-ring/40">
      <SlashMenu commands={commands} />
      <ComposerPrimitive.Attachments>
        {({ attachment }) => (
          <span className="mb-2 mr-2 inline-flex items-center gap-1 rounded-md bg-secondary px-2 py-1 text-xs">
            {attachment.name}
            <AttachmentPrimitive.Remove aria-label={`Remove ${attachment.name}`}>
              <X className="size-3" />
            </AttachmentPrimitive.Remove>
          </span>
        )}
      </ComposerPrimitive.Attachments>
      <ComposerPrimitive.Input
        rows={1}
        maxRows={8}
        placeholder={readOnly ? "You have read-only access" : running ? "Steer the agent while it works…" : "Message the agent (Enter to send, Shift+Enter for a new line)"}
        className="w-full resize-none bg-transparent px-2 py-1.5 text-sm outline-none placeholder:text-muted-foreground disabled:opacity-60"
        aria-label="Message"
      />
      <div className="flex items-center gap-1 pt-1">
        <ComposerPrimitive.AddAttachment asChild>
          <Button type="button" variant="ghost" size="icon" aria-label="Attach an image" className="size-8">
            <Paperclip />
          </Button>
        </ComposerPrimitive.AddAttachment>
        <div className="flex-1" />
        <ComposerPrimitive.Send asChild>
          <Button type="submit" size="sm">
            <Send /> {running ? "Steer" : "Send"}
          </Button>
        </ComposerPrimitive.Send>
      </div>
    </ComposerPrimitive.Root>
  );
}

const dataUrlToAttachment = (name: string, mime: string, url: string): Attachment => ({ name, mime, data: url.slice(url.indexOf(",") + 1) });

export interface ChatViewProps {
  threadId: string;
  settings: RunSettings;
  readOnly: boolean;
  users: (login: string) => Author | undefined;
  self?: Author;
  suggestions?: string[];
  /** Rendered between the transcript and the composer (e.g. nothing yet). */
  models?: Record<string, ModelInfo>;
  onStatus?: (s: { running: boolean; cancel: () => void; title?: string; plan?: { id: string; title: string; status: string }[] }) => void;
  emptyHint?: ReactNode;
  projectId?: string;
}

export function ChatView({ threadId, settings, readOnly, users, self, suggestions, models, onStatus, emptyHint, projectId }: ChatViewProps) {
  const chat = useChat(threadId, settings, users, self);
  const commands = useCommands().data ?? [];
  const { messages, running, send, resume, cancel, interrupt, live, busy, thread } = chat;

  const onNew = useCallback(
    async (m: AppendMessage) => {
      let text = m.content.flatMap((p) => (p.type === "text" ? [p.text] : [])).join("\n").trim();
      const atts: Attachment[] = (m.attachments ?? []).flatMap((a) =>
        a.content.flatMap((c) => (c.type === "image" ? [dataUrlToAttachment(a.name, a.contentType ?? "image/png", c.image)] : [])),
      );
      if (!text && !atts.length) return;
      const cmd = /^\/(\S+)(?:\s+([\s\S]*))?$/.exec(text);
      if (cmd && cmd[1] && commands.some((c) => c.name === cmd[1])) {
        try {
          text = (await api.post<{ prompt: string }>(`/commands/${encodeURIComponent(cmd[1])}/expand`, { args: cmd[2] ?? "" })).prompt;
        } catch {
          toast.error(`Could not expand /${cmd[1]}; sending it as typed.`);
        }
      }
      await send(text, atts).catch(() => undefined);
    },
    [commands, send],
  );

  const runtime = useExternalStoreRuntime<ChatMessage>({
    messages,
    convertMessage: toThreadMessage,
    isRunning: false,
    isDisabled: readOnly,
    onNew,
    adapters: { attachments: adapter },
  });

  useEffect(() => {
    onStatus?.({ running, cancel: () => void cancel(), title: live.title, plan: live.plan });
  }, [running, cancel, live.title, live.plan, onStatus]);

  const t = thread.data;
  useEffect(() => {
    if (t) markSeen(threadId, t.updatedAt);
  }, [threadId, t, messages.length]);

  const usage = useMemo(() => formatUsage(live.usage, settings.model ? models?.[settings.model] : undefined), [live.usage, settings.model, models]);
  const asked = t?.createdBy && interrupt ? t.createdBy : undefined;

  return (
    <MemoryLinkContext.Provider value={projectId}>
    <AssistantRuntimeProvider runtime={runtime}>
      <ThreadPrimitive.Root className="flex h-full min-h-0 flex-col">
        <ThreadPrimitive.Viewport className="min-h-0 flex-1 overflow-y-auto scroll-smooth py-2" aria-live="polite">
          <ThreadPrimitive.Empty>
            <div className="mx-auto max-w-md p-8 text-center text-sm text-muted-foreground">{emptyHint ?? "Send a message to start."}</div>
          </ThreadPrimitive.Empty>
          <ThreadPrimitive.Messages components={{ UserMessage, AssistantMessage }} />
        </ThreadPrimitive.Viewport>
        <div className="space-y-2 border-t bg-background px-3 pb-3 pt-2">
          {interrupt ? <InterruptCard interrupt={interrupt} resume={resume} busy={busy} disabled={readOnly} askedUser={asked} /> : null}
          {!running && !interrupt && suggestions?.length && messages.length < 3 && !readOnly ? (
            <div className="flex flex-wrap gap-2">
              {suggestions.map((s) => (
                <ThreadPrimitive.Suggestion key={s} prompt={s} send asChild>
                  <Button variant="outline" size="sm" className="rounded-full">
                    {s}
                  </Button>
                </ThreadPrimitive.Suggestion>
              ))}
            </div>
          ) : null}
          <div className="flex items-center justify-between gap-2 px-1 text-xs text-muted-foreground">
            <span>{running ? `Running · ${live.steps} steps` : live.phase === "failed" ? "Last run failed" : live.phase === "cancelled" ? "Cancelled" : readOnly ? "Read-only" : "Idle"}</span>
            <span className="truncate">{usage}</span>
          </div>
          <Composer running={running} readOnly={readOnly} commands={commands} />
        </div>
      </ThreadPrimitive.Root>
    </AssistantRuntimeProvider>
    </MemoryLinkContext.Provider>
  );
}


