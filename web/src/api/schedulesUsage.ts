// Queries of the scheduled tasks and the usage page (kept apart from queries.ts: they belong to those two pages only).
import { useLivePoll } from "@/lib/tabActive";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api } from "./client";
import { toastError } from "./queries";
import type { SchedulePreview, ScheduleRun, ScheduleView, UsageCap, UsageView } from "./types";

/** The body of POST/PATCH .../schedules (the handlers decode it from an unnamed struct, so it has no generated type). The server decides who the runs execute as. */
export interface ScheduleInput {
  name?: string;
  prompt?: string;
  cron?: string;
  timezone?: string;
  model?: string;
  policy?: "default" | "readonly";
  timeoutMinutes?: number;
  reuseSession?: boolean;
  enabled?: boolean;
}

/** Where a project's schedules live; without a project, the caller's personal ones. */
export const schedulesPath = (projectId?: string) => (projectId ? `/projects/${projectId}/schedules` : "/me/schedules");
export const schedulesKey = (projectId?: string) => ["schedules", projectId ?? "me"] as const;

export const useSchedules = (projectId?: string) =>
  useQuery({ queryKey: schedulesKey(projectId), queryFn: () => api.get<ScheduleView[]>(schedulesPath(projectId)), retry: false, refetchInterval: useLivePoll(15_000) });

export const useScheduleRuns = (projectId: string | undefined, id: string, enabled: boolean) =>
  useQuery({
    queryKey: [...schedulesKey(projectId), id, "runs"] as const,
    queryFn: () => api.get<ScheduleRun[]>(`${schedulesPath(projectId)}/${id}/runs?limit=20`),
    enabled,
    retry: false,
    refetchInterval: useLivePoll(10_000),
  });

/** The next fire times of an expression in a zone, for the editor. An invalid expression is an error (the server says why). */
export const useSchedulePreview = (cron: string, timezone: string, enabled = true) =>
  useQuery({
    queryKey: ["schedule-preview", cron, timezone] as const,
    queryFn: () => api.get<SchedulePreview>(`/schedules/preview?count=5&cron=${encodeURIComponent(cron)}&timezone=${encodeURIComponent(timezone)}`),
    enabled: enabled && cron.trim() !== "",
    retry: false,
    staleTime: 60_000,
    placeholderData: (prev) => prev,
  });

function useScheduleAction<V, R>(projectId: string | undefined, fn: (v: V) => Promise<R>, success?: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: fn,
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: schedulesKey(projectId) });
      if (success) toast.success(success);
    },
    onError: toastError,
  });
}

/** Creates a schedule, or patches the one with `id`. */
export const useSaveSchedule = (projectId?: string) =>
  useScheduleAction(
    projectId,
    (v: { id?: string; body: ScheduleInput }) => (v.id ? api.patch<ScheduleView>(`${schedulesPath(projectId)}/${v.id}`, v.body) : api.post<ScheduleView>(schedulesPath(projectId), v.body)),
    "Saved",
  );
export const useDeleteSchedule = (projectId?: string) => useScheduleAction(projectId, (id: string) => api.del(`${schedulesPath(projectId)}/${id}`), "Deleted");
export const useRunSchedule = (projectId?: string) => useScheduleAction(projectId, (id: string) => api.post<ScheduleRun>(`${schedulesPath(projectId)}/${id}/run`), "Run started");

// ---- usage ----

export type UsageRange = "day" | "week" | "month" | string;
export type UsageGroup = "day" | "model" | "thread" | "user";

export interface UsageParams {
  /** A project id for the project's usage; without one, the caller's own. */
  projectId?: string;
  range: UsageRange;
  group: UsageGroup;
}

export const usageKey = (p: UsageParams) => ["usage", p.projectId ?? "me", p.range, p.group] as const;
export const useUsage = (p: UsageParams) =>
  useQuery({
    queryKey: usageKey(p),
    queryFn: () => api.get<UsageView>(`/usage?scope=${p.projectId ? `project&project=${encodeURIComponent(p.projectId)}` : "me"}&range=${encodeURIComponent(p.range)}&group=${p.group}`),
    retry: false,
    refetchInterval: useLivePoll(60_000),
  });

/** Sets (0 removes) the monthly cap of the project (an owner) or of the caller. */
export function useSetUsageCap(projectId?: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (monthlyUsd: number) => api.put<UsageCap>(projectId ? `/projects/${projectId}/usage/cap` : "/me/usage/cap", { monthlyUsd }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["usage"] });
      toast.success("Cap saved");
    },
    onError: toastError,
  });
}
