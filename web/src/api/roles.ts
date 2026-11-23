// Roles: named agent configurations of a project (or of the user). Kept apart from queries.ts so the page and the chat's @ picker
// share one set of hooks.
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api, isUnsupported } from "./client";
import { toastError } from "./queries";
import type { RolePreview, RoleVersion, RoleView } from "./types";

/** What the editor sends; `version` is the one the editor started from (a stale one is refused with 409). */
export interface RoleBody {
  name: string;
  title: string;
  description: string;
  prompt: string;
  model: string;
  reasoningEffort: string;
  toolProfile: string;
  allowTools: string[];
  denyTools: string[];
  skills: string[];
  mcp: string[];
  permissionMode: string;
  enabled: boolean;
  version?: number;
}

export const rolesKey = (pid?: string) => ["roles", pid ?? "user"] as const;
const base = (pid?: string) => (pid ? `/projects/${pid}/roles` : "/me/roles");
const path = (pid: string | undefined, name?: string, suffix = "") => `${base(pid)}${name ? `/${encodeURIComponent(name)}` : ""}${suffix}`;

/** The roles in reach (builtin, the project's, the caller's own), or null when this server has no roles (404/501). */
export const useRoles = (pid?: string, enabled = true) =>
  useQuery({
    queryKey: rolesKey(pid),
    queryFn: async () => {
      try {
        return await api.get<RoleView[]>(base(pid));
      } catch (e) {
        if (isUnsupported(e)) return null;
        throw e;
      }
    },
    enabled,
    retry: false,
    staleTime: 30_000,
  });

function useRoleAction<V, R>(pid: string | undefined, fn: (v: V) => Promise<R>, success?: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: fn,
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: rolesKey(pid) });
      if (success) toast.success(success);
    },
    onError: toastError,
  });
}

export const useCreateRole = (pid?: string) => useRoleAction(pid, (b: RoleBody) => api.post<RoleView>(path(pid), b), "Role created");
export const useUpdateRole = (pid?: string) => useRoleAction(pid, (b: RoleBody) => api.put<RoleView>(path(pid, b.name), b), "Role saved");
export const useDeleteRole = (pid?: string) => useRoleAction(pid, (name: string) => api.del(path(pid, name)), "Role removed");
export const useDuplicateRole = (pid?: string) => useRoleAction(pid, (v: { name: string; as?: string }) => api.post<RoleView>(path(pid, v.name, "/duplicate"), v.as ? { name: v.as } : {}), "Role copied");
export const useRestoreRole = (pid?: string) => useRoleAction(pid, (v: { name: string; version: number }) => api.post<RoleView>(path(pid, v.name, "/restore"), { version: v.version }), "Version restored");

export const useRoleHistory = (pid: string | undefined, name: string | undefined, version: number | undefined) =>
  useQuery({ queryKey: [...rolesKey(pid), name, "history", version], queryFn: () => api.get<RoleVersion[]>(path(pid, name, "/history")), enabled: !!name, retry: false });

/** What a run in the role would get, rendered by the server from the draft; nothing is stored. */
export const usePreviewRole = (pid?: string) => useMutation({ mutationFn: (b: Partial<RoleBody>) => api.post<RolePreview>(path(pid, undefined, "/preview"), b), onError: toastError });
