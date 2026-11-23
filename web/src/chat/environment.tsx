// The runtime environment of a conversation: a sandbox profile the server offers (GET /environments). Chosen for a
// thread before it has a workspace, frozen after that; switching then opens a new session in the other environment.
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { Server } from "lucide-react";
import { useState } from "react";
import { api } from "@/api/client";
import { toastError } from "@/api/queries";
import type { EnvironmentList, Thread, ThreadEnvironment } from "@/api/types";
import { Button } from "@/components/ui/button";

export const useEnvironments = () => useQuery({ queryKey: ["environments"], queryFn: () => api.get<EnvironmentList>("/environments"), staleTime: 300_000, retry: false });

export const threadEnvironmentKey = (threadId: string) => ["environment", threadId] as const;
export const useThreadEnvironment = (threadId: string, enabled = true) =>
  useQuery({ queryKey: threadEnvironmentKey(threadId), queryFn: () => api.get<ThreadEnvironment>(`/threads/${threadId}/environment`), enabled, retry: false });

/**
 * The environment choice of the session header. Hidden unless the server says it is selectable (several environments, or
 * ENVIRONMENT_SELECTOR is set).
 */
export function EnvironmentControl({ threadId, projectId, readOnly }: { threadId: string; projectId?: string; readOnly?: boolean }) {
  const envs = useEnvironments().data;
  const selectable = !!envs?.selectable;
  const current = useThreadEnvironment(threadId, selectable).data;
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [wanted, setWanted] = useState<string | null>(null);
  const set = useMutation({
    mutationFn: (environment: string) => api.put<ThreadEnvironment>(`/threads/${threadId}/environment`, { environment }),
    onSuccess: (r) => qc.setQueryData(threadEnvironmentKey(threadId), r),
    onError: toastError,
  });
  const reopen = useMutation({
    mutationFn: async (environment: string) => {
      const t = await api.post<Thread>("/threads", { title: "", projectId, environment });
      // A shared-project server creates the thread itself and ignores the field: say it again, the thread is still empty.
      await api.put<ThreadEnvironment>(`/threads/${t.id}/environment`, { environment });
      return t;
    },
    onSuccess: (t) => {
      void qc.invalidateQueries({ queryKey: ["overview"] });
      void navigate({ to: "/t/$threadId", params: { threadId: t.id } });
    },
    onError: toastError,
  });
  if (!envs || !selectable || !current) return null;
  const value = current.environment || envs.default;
  const chosen = envs.environments.find((e) => e.name === value);
  const target = envs.environments.find((e) => e.name === wanted);
  return (
    <div className="flex flex-wrap items-center gap-2" data-testid="environment-control">
      <label className="flex items-center gap-1.5">
        <Server className="size-3.5 text-muted-foreground" />
        <span className="text-muted-foreground">Environment</span>
        <select
          aria-label="Runtime environment"
          className="h-7 rounded-md border bg-background px-2 text-xs"
          value={value}
          disabled={readOnly || set.isPending}
          onChange={(e) => {
            const next = e.target.value;
            if (next === value) return;
            if (current.frozen) setWanted(next);
            else set.mutate(next);
          }}
        >
          {envs.environments.map((e) => (
            <option key={e.name} value={e.name}>
              {e.name} ({e.kind})
            </option>
          ))}
        </select>
      </label>
      {chosen ? (
        <span className="text-muted-foreground" title={`${chosen.description} Egress: ${chosen.egress}.`}>
          egress: {chosen.egress}
        </span>
      ) : null}
      {current.frozen && !target ? <span className="text-muted-foreground">Fixed once the session has a workspace.</span> : null}
      {current.frozen && target ? (
        <span className="flex items-center gap-2 rounded-md bg-muted px-2 py-1">
          This session already has a workspace.
          <Button type="button" size="sm" className="h-6 px-2 text-xs" disabled={reopen.isPending} onClick={() => reopen.mutate(target.name)}>
            Open a new session in {target.name}
          </Button>
          <Button type="button" size="sm" variant="ghost" className="h-6 px-2 text-xs" onClick={() => setWanted(null)}>
            Cancel
          </Button>
        </span>
      ) : null}
    </div>
  );
}
