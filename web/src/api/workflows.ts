// Workflows: the procedures (SOPs) of a project and their runs. Kept apart from queries.ts like roles.ts; the live feed
// invalidates ["workflows"] when a workflow, a run or a gate changes.
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { ApiError, api, isUnsupported } from "./client";
import { fk } from "./factory";
import { toastError } from "./queries";
import type { Workflow, WorkflowIssues, WorkflowNode, WorkflowRun, WorkflowRunDetail, WorkflowTemplate, WorkflowVersion } from "./types";

export type { Workflow, WorkflowIssues, WorkflowNode, WorkflowRun, WorkflowRunDetail, WorkflowTemplate, WorkflowVersion };

/** What the designer sends; `version` is the one it started from (a stale one is refused with 409). */
export interface WorkflowBody {
  name: string;
  description: string;
  inputs: Workflow["inputs"];
  nodes: WorkflowNode[];
  version?: number;
}

export const wk = {
  all: ["workflows"] as const,
  list: (pid: string) => ["workflows", pid, "list"] as const,
  one: (pid: string, id: string) => ["workflows", pid, "one", id] as const,
  history: (pid: string, id: string) => ["workflows", pid, "history", id] as const,
  runs: (pid: string) => ["workflows", pid, "runs"] as const,
  run: (pid: string, id: string) => ["workflows", pid, "run", id] as const,
  templates: ["workflows", "templates"] as const,
};

/** The server's validation answer, kept on the error so the designer can list the issues. */
export const issuesOf = (e: unknown): WorkflowIssues["issues"] => (e instanceof ApiError && e.issues ? e.issues : []);

const base = (pid: string) => `/projects/${pid}/workflows`;

/** The project's workflows, or null when this server has no workflows (404/501). */
export const useWorkflows = (pid: string, enabled = true) =>
  useQuery({
    queryKey: wk.list(pid),
    queryFn: async () => {
      try {
        return await api.get<Workflow[]>(base(pid));
      } catch (e) {
        if (isUnsupported(e)) return null;
        throw e;
      }
    },
    retry: false,
    staleTime: 15_000,
    enabled: enabled && !!pid,
  });

export const useWorkflowTemplates = (enabled = true) =>
  useQuery({ queryKey: wk.templates, queryFn: () => api.get<WorkflowTemplate[]>("/workflow-templates"), enabled, retry: false, staleTime: Infinity });

export const useWorkflowHistory = (pid: string, id: string | undefined, version: number | undefined) =>
  useQuery({ queryKey: [...wk.history(pid, id ?? ""), version], queryFn: () => api.get<WorkflowVersion[]>(`${base(pid)}/${id}/history`), enabled: !!id, retry: false });

function useWorkflowAction<V, R>(fn: (v: V) => Promise<R>, success?: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: fn,
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: wk.all });
      await qc.invalidateQueries({ queryKey: fk.inbox });
      if (success) toast.success(success);
    },
    // A rejected graph is shown by the designer next to its steps: no toast for it.
    onError: (e) => (issuesOf(e).length ? undefined : toastError(e)),
  });
}

export const useCreateWorkflow = (pid: string) => useWorkflowAction((b: WorkflowBody) => api.post<Workflow>(base(pid), b), "Workflow created");
export const useUpdateWorkflow = (pid: string) => useWorkflowAction((v: { id: string; body: WorkflowBody }) => api.put<Workflow>(`${base(pid)}/${v.id}`, v.body), "Workflow saved");
export const useDeleteWorkflow = (pid: string) => useWorkflowAction((id: string) => api.del(`${base(pid)}/${id}`), "Workflow deleted");
export const usePublishWorkflow = (pid: string) => useWorkflowAction((v: { id: string; version?: number }) => api.post<Workflow>(`${base(pid)}/${v.id}/publish`, { version: v.version }), "Workflow published");
export const useDuplicateWorkflow = (pid: string) => useWorkflowAction((v: { id: string; name?: string }) => api.post<Workflow>(`${base(pid)}/${v.id}/duplicate`, v.name ? { name: v.name } : {}), "Workflow copied");
export const useRestoreWorkflow = (pid: string) => useWorkflowAction((v: { id: string; version: number }) => api.post<Workflow>(`${base(pid)}/${v.id}/restore`, { version: v.version }), "Version restored as a draft");
export const useFromTemplate = (pid: string) => useWorkflowAction((v: { template: string; name?: string }) => api.post<Workflow>(`${base(pid)}/from-template`, v), "Template copied to the project");

/** Checks a graph on the server without storing it (roles, subworkflows, references). */
export const validateWorkflow = (pid: string, body: WorkflowBody, id?: string) =>
  api.post<WorkflowIssues>(`${base(pid)}/validate${id ? `?id=${id}` : ""}`, body).then((r) => r.issues);

// ---- runs ----

export const useWorkflowRuns = (pid: string, enabled = true) =>
  useQuery({
    queryKey: wk.runs(pid),
    queryFn: async () => {
      try {
        return await api.get<WorkflowRun[]>(`/projects/${pid}/workflow-runs?limit=50`);
      } catch (e) {
        if (isUnsupported(e)) return null;
        throw e;
      }
    },
    enabled,
    retry: false,
    refetchInterval: 30_000,
  });

export const useWorkflowRun = (pid: string, id: string | undefined) =>
  useQuery({ queryKey: wk.run(pid, id ?? ""), queryFn: () => api.get<WorkflowRunDetail>(`/projects/${pid}/workflow-runs/${id}`), enabled: !!id, refetchInterval: 10_000 });

export interface RunInput {
  workflowId: string;
  inputs: Record<string, string>;
  requirementId?: string;
}
export const useStartRun = (pid: string) => useWorkflowAction((b: RunInput) => api.post<WorkflowRun>(`/projects/${pid}/workflow-runs`, b), "Run started");

export type RunAction = "pause" | "resume" | "cancel" | "rerun" | "retry" | "takeover";
export interface RunActionInput {
  runId: string;
  action: RunAction;
  nodeId?: string;
  feedback?: string;
  result?: string;
}
const ACTION_DONE: Record<RunAction, string> = {
  pause: "Run paused",
  resume: "Run resumed",
  cancel: "Run cancelled",
  rerun: "Running again from that step",
  retry: "Step retried",
  takeover: "Step completed by you",
};
export const useRunAction = () => {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: RunActionInput) => api.post<WorkflowRun>(`/workflow-runs/${v.runId}/${v.action}`, { nodeId: v.nodeId, feedback: v.feedback, result: v.result }),
    onSuccess: async (_, v) => {
      await qc.invalidateQueries({ queryKey: wk.all });
      await qc.invalidateQueries({ queryKey: fk.all });
      toast.success(ACTION_DONE[v.action]);
    },
    onError: toastError,
  });
};

export type GateAction = "approve" | "send-back" | "add-context";
export const useGateAction = () => {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { runId: string; nodeId: string; action: GateAction; text?: string }) =>
      api.post<WorkflowRun>(`/workflow-runs/${v.runId}/nodes/${v.nodeId}/${v.action}`, v.action === "send-back" ? { feedback: v.text } : v.action === "add-context" ? { text: v.text } : { comment: v.text }),
    onSuccess: async (_, v) => {
      await qc.invalidateQueries({ queryKey: wk.all });
      await qc.invalidateQueries({ queryKey: fk.inbox });
      toast.success(v.action === "approve" ? "Approved" : v.action === "send-back" ? "Sent back" : "Context added");
    },
    onError: toastError,
  });
};
