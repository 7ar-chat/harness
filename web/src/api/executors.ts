// External coding-agent executors (Claude Code, Codex, OpenCode, a project's own command) and the credentials a user seals for
// them. The credentials are write-only: the server never returns them, only whether the caller has set any (`configured`).
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api, isUnsupported } from "./client";
import { toastError } from "./queries";
import type { ExecutorView } from "./types";

/** What the project's owner sends to define a custom executor. */
export interface ExecutorBody {
  name: string;
  title: string;
  description: string;
  command: string;
  binary: string;
  installCommand: string;
  credentialEnv: string[];
  parser: string;
  modeFlags: Record<string, string>;
  enabled: boolean;
}

export const executorsKey = (pid?: string) => ["executors", pid ?? "user"] as const;
const listPath = (pid?: string) => (pid ? `/projects/${pid}/executors` : "/executors");

/** The executors in reach (the builtin ones, and the project's), or null when this server has none (404/501). */
export const useExecutors = (pid?: string, enabled = true) =>
  useQuery({
    queryKey: executorsKey(pid),
    queryFn: async () => {
      try {
        return await api.get<ExecutorView[]>(listPath(pid));
      } catch (e) {
        if (isUnsupported(e)) return null;
        throw e;
      }
    },
    enabled,
    retry: false,
    staleTime: 30_000,
  });

function useExecutorAction<V, R>(fn: (v: V) => Promise<R>, success?: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: fn,
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ["executors"] });
      if (success) toast.success(success);
    },
    onError: toastError,
  });
}

const projectQuery = (pid?: string) => (pid ? `?project=${encodeURIComponent(pid)}` : "");

/** Seals credentials for the caller: `env` maps a variable name to its value; an empty value removes that variable. */
export const usePutExecutorCredentials = () =>
  useExecutorAction(
    (v: { name: string; env: Record<string, string>; project?: string }) =>
      api.put<ExecutorView>(`/me/executors/${encodeURIComponent(v.name)}/credentials${projectQuery(v.project)}`, { env: v.env }),
    "Credentials saved",
  );
export const useDeleteExecutorCredentials = () =>
  useExecutorAction((name: string) => api.del(`/me/executors/${encodeURIComponent(name)}/credentials`), "Credentials removed");

export const useSaveExecutor = (pid: string) =>
  useExecutorAction(
    (v: { body: ExecutorBody; update: boolean }) =>
      v.update ? api.put<ExecutorView>(`/projects/${pid}/executors/${encodeURIComponent(v.body.name)}`, v.body) : api.post<ExecutorView>(`/projects/${pid}/executors`, v.body),
    "Executor saved",
  );
export const useDeleteExecutor = (pid: string) => useExecutorAction((name: string) => api.del(`/projects/${pid}/executors/${encodeURIComponent(name)}`), "Executor removed");
