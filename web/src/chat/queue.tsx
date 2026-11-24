// The queue of messages sent while a reply is running: kept by the server (so it survives a reload), shown above the
// composer, sent one after the other when the reply ends. Each item can steer the running reply now, or be removed.
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ListPlus, Paperclip, Trash2, Zap } from "lucide-react";
import { useEffect, useRef } from "react";
import { api } from "@/api/client";
import { qk, toastError } from "@/api/queries";
import type { QueueItem, QueueSendResult } from "@/api/types";
import { useLivePollFn } from "@/lib/tabActive";
import { Button } from "@/components/ui/button";

export const queueKey = (threadId: string) => ["queue", threadId] as const;

/** How many messages wait on a thread, for the tab of its session (the same query as the chat's own, so it costs nothing more). */
export function useQueuedCount(threadId: string, enabled: boolean): number {
  const q = useQuery({
    queryKey: queueKey(threadId),
    queryFn: () => api.get<QueueItem[]>(`/threads/${threadId}/queue`),
    enabled: enabled && !!threadId,
    staleTime: 20_000,
    retry: false,
  });
  return q.data?.length ?? 0;
}

export function useQueue(threadId: string) {
  const qc = useQueryClient();
  // While something waits, look again: the server sends it as soon as the reply ends, and the page must follow.
  // A hidden tab does not look; it refetches when it is shown.
  const interval = useLivePollFn((query: { state: { data?: QueueItem[] } }) => (query.state.data?.length ? 2000 : false));
  const q = useQuery({
    queryKey: queueKey(threadId),
    queryFn: () => api.get<QueueItem[]>(`/threads/${threadId}/queue`),
    refetchInterval: interval,
    retry: false,
  });
  const items = q.data ?? [];
  const prev = useRef(0);
  useEffect(() => {
    // A message left the queue (sent by the server): the thread has a new run to follow.
    if (items.length < prev.current) void qc.invalidateQueries({ queryKey: qk.thread(threadId) });
    prev.current = items.length;
  }, [items.length, qc, threadId]);
  const refresh = () => {
    void qc.invalidateQueries({ queryKey: queueKey(threadId) });
    void qc.invalidateQueries({ queryKey: qk.thread(threadId) });
  };
  const remove = useMutation({ mutationFn: (id: string) => api.del(`/threads/${threadId}/queue/${id}`), onSuccess: refresh, onError: toastError });
  const sendNow = useMutation({ mutationFn: (id: string) => api.post<QueueSendResult>(`/threads/${threadId}/queue/${id}/send`), onSuccess: refresh, onError: toastError });
  const discard = useMutation({ mutationFn: () => api.del(`/threads/${threadId}/queue`), onSuccess: refresh, onError: toastError });
  return {
    items,
    remove: (id: string) => remove.mutate(id),
    sendNow: (id: string) => sendNow.mutate(id),
    discardAll: () => discard.mutate(),
    pending: remove.isPending || sendNow.isPending || discard.isPending,
  };
}

export function QueueCard({ items, running, readOnly, remove, sendNow, discardAll, pending }: { items: QueueItem[]; running: boolean; readOnly: boolean } & Omit<ReturnType<typeof useQueue>, "items">) {
  if (!items.length) return null;
  return (
    <section aria-label="Queued messages" className="rounded-lg border bg-muted/40 text-sm">
      <header className="flex items-center gap-2 px-3 py-1.5 text-xs text-muted-foreground">
        <ListPlus className="size-3.5" />
        <span className="font-medium text-foreground">{items.length === 1 ? "1 message queued" : `${items.length} messages queued`}</span>
        <span>{running ? "Sent one by one when the reply ends." : "Waiting: the last reply did not complete."}</span>
        {!readOnly ? (
          <span className="ml-auto flex gap-1">
            {!running ? (
              <Button type="button" variant="ghost" size="sm" className="h-6 px-2 text-xs" disabled={pending} onClick={() => sendNow(items[0]!.id)}>
                <Zap className="size-3" /> Send now
              </Button>
            ) : null}
            <Button type="button" variant="ghost" size="sm" className="h-6 px-2 text-xs" disabled={pending} onClick={discardAll}>
              <Trash2 className="size-3" /> Discard all
            </Button>
          </span>
        ) : null}
      </header>
      <ol className="divide-y">
        {items.map((it, i) => (
          <li key={it.id} className="flex items-center gap-2 px-3 py-1.5">
            <span className="w-4 shrink-0 text-xs text-muted-foreground">{i + 1}</span>
            <span className="min-w-0 flex-1 truncate" title={it.text}>
              {it.text}
            </span>
            {it.attachments?.length ? (
              <span className="flex shrink-0 items-center gap-1 text-xs text-muted-foreground" title={it.attachments.map((a) => a.name).join(", ")}>
                <Paperclip className="size-3" aria-hidden />
                {it.attachments.length === 1 ? it.attachments[0]!.name : `${it.attachments.length} images`}
              </span>
            ) : null}
            {!readOnly ? (
              <>
                <Button type="button" variant="ghost" size="sm" className="h-7 px-2 text-xs" disabled={pending} onClick={() => sendNow(it.id)} aria-label={`${running ? "Steer now" : "Send now"}: ${it.text}`}>
                  <Zap className="size-3" /> {running ? "Steer now" : "Send now"}
                </Button>
                <Button type="button" variant="ghost" size="icon" className="size-7" disabled={pending} onClick={() => remove(it.id)} aria-label={`Remove: ${it.text}`}>
                  <Trash2 className="size-3.5" />
                </Button>
              </>
            ) : null}
          </li>
        ))}
      </ol>
    </section>
  );
}
