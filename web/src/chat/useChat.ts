// Everything the chat needs for one thread: the persisted thread, the live run stream,
// sending / steering / resuming / cancelling.
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useMemo, useReducer, useRef, useState } from "react";
import { toast } from "sonner";
import { api } from "@/api/client";
import { qk, threadQuery, toastError } from "@/api/queries";
import type { Attachment, Author, ResumeRequest, Run, RunRequest, Thread } from "@/api/types";
import { initialLive, reduceEvents, type AguiEvent, type Interrupt, type LiveState } from "@/lib/agui";
import { openSse } from "@/lib/sse";
import { messagesFromLive, messagesFromThread, type ChatMessage } from "./model";

type Action = { type: "begin"; runId: string } | { type: "event"; ev: AguiEvent } | { type: "done" } | { type: "clearInterrupt" };

function liveReducer(st: LiveState, a: Action): LiveState {
  switch (a.type) {
    case "begin":
      return { ...initialLive, usage: st.usage, plan: st.plan, runId: a.runId, phase: "running" };
    case "event":
      return reduceEvents(st, a.ev);
    case "done":
      return { ...initialLive, usage: st.usage, plan: st.plan, title: st.title, phase: st.phase, interrupt: st.interrupt };
    case "clearInterrupt":
      return { ...st, interrupt: undefined, phase: "idle" };
  }
}

export interface RunSettings {
  model?: string;
  interactive: boolean;
  mode: "build" | "plan";
  sandbox?: "auto" | "on" | "off";
  reasoningEffort?: string;
}

/** The interrupt waiting for an answer: from the live stream, or from the thread JSON after a reload. */
function pendingOf(t: Thread | undefined, live: LiveState): Interrupt | undefined {
  if (live.interrupt) return live.interrupt;
  if (t?.pendingApproval) {
    const a = t.pendingApproval;
    return { id: a.id, reason: "tool_approval", approval: { id: a.id, toolName: a.toolName, toolInputPreview: a.toolInputPreview }, toolCallId: a.toolCallId };
  }
  if (t?.pendingQuestion) {
    const q = t.pendingQuestion;
    return { id: q.interruptId, reason: "ask_user", message: q.question, responseSchema: { options: q.options, allowFreeText: q.allowFreeText } };
  }
  return undefined;
}

export function useChat(threadId: string, settings: RunSettings, users: (login: string) => Author | undefined, self?: Author) {
  const qc = useQueryClient();
  const q = useQuery(threadQuery(threadId));
  const [live, dispatch] = useReducer(liveReducer, initialLive);
  const [pending, setPending] = useState<ChatMessage | null>(null);
  const [busy, setBusy] = useState(false);
  const closeRef = useRef<() => void>();
  const followingRef = useRef<string | null>(null);
  const lastDoneRef = useRef<string | null>(null);

  const stop = useCallback(() => {
    closeRef.current?.();
    closeRef.current = undefined;
    followingRef.current = null;
  }, []);

  const follow = useCallback(
    (runId: string) => {
      if (followingRef.current === runId) return;
      stop();
      followingRef.current = runId;
      dispatch({ type: "begin", runId });
      let seq = 0;
      const finish = async () => {
        if (followingRef.current !== runId) return;
        stop();
        lastDoneRef.current = runId;
        try {
          await qc.refetchQueries({ queryKey: qk.thread(threadId) });
        } catch {
          /* shown by the query's own error state */
        }
        void qc.invalidateQueries({ queryKey: qk.overview });
        void qc.invalidateQueries({ queryKey: ["files", threadId] });
        dispatch({ type: "done" });
      };
      closeRef.current = openSse({
        url: () => `/api/runs/${runId}/events?after=${seq}`,
        maxRetries: 6,
        onGiveUp: () => void finish(),
        onMessage: (m) => {
          if (!m.data) return;
          if (m.id) seq = Number(m.id) || seq;
          let ev: AguiEvent;
          try {
            ev = JSON.parse(m.data) as AguiEvent;
          } catch {
            return;
          }
          dispatch({ type: "event", ev });
          if (ev.type === "RUN_FINISHED" || ev.type === "RUN_ERROR") void finish();
        },
      });
    },
    [qc, stop, threadId],
  );

  useEffect(() => stop, [stop]);

  // Someone else (or an earlier visit) started a run: follow it.
  const activeId = q.data?.activeRun?.id;
  useEffect(() => {
    if (activeId && followingRef.current !== activeId && lastDoneRef.current !== activeId) follow(activeId);
  }, [activeId, follow]);

  const running = live.phase === "running" || !!activeId;

  const send = useCallback(
    async (text: string, attachments: Attachment[] = []) => {
      const runId = followingRef.current ?? activeId;
      setBusy(true);
      try {
        if (runId) {
          await api.post(`/runs/${runId}/steer`, { text });
          return;
        }
        setPending({ id: "pending", role: "user", parts: [{ kind: "text", id: "p", text }], author: self });
        dispatch({ type: "clearInterrupt" });
        const body: RunRequest = {
          message: text,
          model: settings.model || undefined,
          reasoningEffort: settings.reasoningEffort || undefined,
          interactive: settings.interactive,
          mode: settings.mode,
          sandbox: settings.sandbox,
          attachments: attachments.length ? attachments : undefined,
        };
        const run = await api.post<Run>(`/threads/${threadId}/runs`, body);
        follow(run.id);
        await qc.refetchQueries({ queryKey: qk.thread(threadId) });
        void qc.invalidateQueries({ queryKey: qk.overview });
      } catch (e) {
        toastError(e);
        throw e;
      } finally {
        setPending(null);
        setBusy(false);
      }
    },
    [activeId, follow, qc, self, settings, threadId],
  );

  const resume = useCallback(
    async (body: Omit<ResumeRequest, "model" | "interactive" | "mode">) => {
      setBusy(true);
      try {
        const run = await api.post<Run>(`/threads/${threadId}/resume`, { ...body, model: settings.model || undefined, interactive: settings.interactive, mode: settings.mode });
        dispatch({ type: "clearInterrupt" });
        follow(run.id);
        void qc.invalidateQueries({ queryKey: qk.thread(threadId) });
        void qc.invalidateQueries({ queryKey: qk.overview });
      } catch (e) {
        toastError(e);
      } finally {
        setBusy(false);
      }
    },
    [follow, qc, settings.interactive, settings.mode, settings.model, threadId],
  );

  const cancel = useCallback(async () => {
    const runId = followingRef.current ?? activeId;
    if (!runId) return;
    try {
      await api.post(`/runs/${runId}/cancel`);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "Could not cancel");
    }
  }, [activeId]);

  const messages = useMemo(() => {
    const persisted = messagesFromThread(q.data?.messages ?? [], live.parts.length ? live.runId : undefined);
    const streamed = messagesFromLive(live, live.phase === "running", users);
    return [...persisted, ...streamed, ...(pending ? [pending] : [])];
  }, [q.data?.messages, live, pending, users]);

  return { thread: q, live, messages, running, busy, interrupt: pendingOf(q.data, live), send, resume, cancel };
}
