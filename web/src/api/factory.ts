// The agent factory over HTTP (tasks, triggers, the spend report, the learning views) and the "Needs review" inbox.
import { useLivePoll } from "@/lib/tabActive";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api, ApiError, isUnsupported } from "./client";
import { toastError } from "./queries";
import type { Budget, FactoryReport, FactoryState, FactoryTask, FactoryTaskEvent, FactoryTrigger, InboxDoc } from "./types";

export type { FactoryReport, FactoryState, FactoryTask, FactoryTaskEvent, FactoryTrigger, InboxDoc };

export const fk = {
  all: ["factory"] as const,
  tasks: (pid: string) => ["factory", pid, "tasks"] as const,
  task: (pid: string, tid: string) => ["factory", pid, "task", tid] as const,
  triggers: (pid: string) => ["factory", pid, "triggers"] as const,
  report: (pid: string, days: number) => ["factory", pid, "report", days] as const,
  learning: (pid: string, view: string) => ["factory", pid, "learning", view] as const,
  inbox: ["needs-review"] as const,
};

export interface TaskDetail {
  task: FactoryTask;
  events: FactoryTaskEvent[];
}

/** The body of POST /projects/{pid}/tasks. */
export interface TaskInput {
  title: string;
  spec: string;
  role?: string;
  model?: string;
  policy?: string;
  budget?: Budget;
  maxAttempts?: number;
  autoMerge?: boolean;
  dependsOn?: string[];
}

/** The body of POST /projects/{pid}/triggers. */
export interface TriggerInput {
  name: string;
  kind: "cron" | "webhook";
  schedule?: string;
  event?: string;
  match?: Record<string, string>;
  enabled: boolean;
  template: { title: string; spec: string; role?: string; policy?: string; model?: string; baseBranch?: string; budget?: Budget };
}

export interface LearningCluster {
  signature: string;
  count: number;
  tasks: string[];
  titles?: string[];
  sample: string;
  usd: number;
  firstSeen: string;
  lastSeen: string;
}
export interface LearningStanding {
  variant: string;
  n: number;
  wins: number;
  rate: number;
  lo: number;
  hi: number;
  meanUsd: number;
}
export interface LearningExperiment {
  standings: LearningStanding[];
  winner?: string;
  [k: string]: unknown;
}

const noRetryOnClientError = (n: number, e: unknown) => !(e instanceof ApiError && e.status < 500) && n < 2;

export const useTasks = (pid: string) =>
  useQuery({ queryKey: fk.tasks(pid), queryFn: () => api.get<FactoryTask[]>(`/projects/${pid}/tasks?limit=500`), retry: noRetryOnClientError, refetchInterval: useLivePoll(30_000) });

export const useTask = (pid: string, tid: string | undefined) =>
  useQuery({ queryKey: fk.task(pid, tid ?? ""), queryFn: () => api.get<TaskDetail>(`/projects/${pid}/tasks/${tid}`), enabled: !!tid, refetchInterval: useLivePoll(15_000) });

function useFactoryAction<V, R>(fn: (v: V) => Promise<R>, success?: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: fn,
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: fk.all });
      void qc.invalidateQueries({ queryKey: fk.inbox });
      if (success) toast.success(success);
    },
    onError: toastError,
  });
}

export const useCreateTask = (pid: string) => useFactoryAction((b: TaskInput) => api.post<FactoryTask>(`/projects/${pid}/tasks`, b), "Task created");
export const useCancelTask = (pid: string) => useFactoryAction((tid: string) => api.post(`/projects/${pid}/tasks/${tid}/cancel`), "Task cancelled");
export const useResumeTask = (pid: string) =>
  useFactoryAction((v: { tid: string; feedback?: string; budget?: Budget }) => api.post(`/projects/${pid}/tasks/${v.tid}/resume`, { feedback: v.feedback, budget: v.budget }), "Task resumed");

export const useTriggers = (pid: string) => useQuery({ queryKey: fk.triggers(pid), queryFn: () => api.get<FactoryTrigger[]>(`/projects/${pid}/triggers`), retry: noRetryOnClientError });
export const useCreateTrigger = (pid: string) => useFactoryAction((b: TriggerInput) => api.post<FactoryTrigger>(`/projects/${pid}/triggers`, b), "Trigger created");
export const useDeleteTrigger = (pid: string) => useFactoryAction((id: string) => api.del(`/projects/${pid}/triggers/${id}`), "Trigger deleted");

export const useFactoryReport = (pid: string, days: number) =>
  useQuery({ queryKey: fk.report(pid, days), queryFn: () => api.get<FactoryReport>(`/projects/${pid}/factory/report?days=${days}`), retry: noRetryOnClientError });

/** A learning view; null when the server has the learning feature off (404). */
export const useLearning = <T,>(pid: string, view: "clusters" | "experiment") =>
  useQuery({
    queryKey: fk.learning(pid, view),
    queryFn: async () => {
      try {
        return await api.get<T>(`/projects/${pid}/factory/learning?view=${view}`);
      } catch (e) {
        if (isUnsupported(e)) return null;
        throw e;
      }
    },
    retry: false,
  });

/** Everything that waits on the caller across projects; refreshed by the live feed. */
export const useNeedsReview = () => useQuery({ queryKey: fk.inbox, queryFn: () => api.get<InboxDoc>("/needs-review"), staleTime: 15_000, refetchInterval: 60_000 });

export const TASK_STATE_LABEL: Record<FactoryState, string> = {
  pending: "Waiting for dependencies",
  ready: "Ready",
  running: "Running",
  review: "In review",
  blocked: "Blocked",
  done: "Done",
  failed: "Failed",
  cancelled: "Cancelled",
};
export const isTerminalState = (s: FactoryState) => s === "done" || s === "failed" || s === "cancelled";
