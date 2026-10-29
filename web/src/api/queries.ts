import { queryOptions, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api, ApiError, isUnsupported } from "./client";
import type {
  AuditEntry, Command, GithubStatus, Invite, Me, Member, MemoryEntry, MemoryInput, MemoryList, Models, Overview,
  Project, ProjectWithRole, Role, Thread, WorkspaceFile,
} from "./types";

export const qk = {
  me: ["me"] as const,
  overview: ["overview"] as const,
  models: ["models"] as const,
  commands: ["commands"] as const,
  thread: (id: string) => ["thread", id] as const,
  supervisor: ["supervisor"] as const,
  project: (id: string) => ["project", id] as const,
  members: (id: string) => ["project", id, "members"] as const,
  invites: (id: string) => ["project", id, "invites"] as const,
  audit: (id: string) => ["project", id, "audit"] as const,
  github: (id: string) => ["project", id, "github"] as const,
  memory: (project?: string) => ["memory", project ?? "user"] as const,
  files: (thread: string, path: string) => ["files", thread, path] as const,
};

export const meQuery = queryOptions({ queryKey: qk.me, queryFn: () => api.get<Me>("/me"), staleTime: 60_000, retry: (n, e) => !(e instanceof ApiError && e.status < 500) && n < 2 });
export const overviewQuery = queryOptions({ queryKey: qk.overview, queryFn: () => api.get<Overview>("/overview"), staleTime: 30_000 });
export const modelsQuery = queryOptions({ queryKey: qk.models, queryFn: () => api.get<Models>("/models"), staleTime: 300_000, retry: 1 });
export const threadQuery = (id: string) => queryOptions({ queryKey: qk.thread(id), queryFn: () => api.get<Thread>(`/threads/${id}`), retry: (n, e) => !(e instanceof ApiError && e.status < 500) && n < 2 });
export const supervisorQuery = queryOptions({
  queryKey: qk.supervisor,
  queryFn: async () => {
    try {
      return await api.get<Thread>("/supervisor");
    } catch (e) {
      if (isUnsupported(e)) return null;
      throw e;
    }
  },
  retry: false,
  staleTime: 60_000,
});

export const useMe = () => useQuery(meQuery);
export const useOverview = () => useQuery(overviewQuery);
export const useModels = () => useQuery(modelsQuery);
export const useCommands = () =>
  useQuery({
    queryKey: qk.commands,
    queryFn: async () => {
      try {
        return await api.get<Command[]>("/commands");
      } catch (e) {
        if (isUnsupported(e)) return [];
        throw e;
      }
    },
    staleTime: 300_000,
    retry: false,
  });
export const useProject = (id: string) => useQuery({ queryKey: qk.project(id), queryFn: () => api.get<ProjectWithRole>(`/projects/${id}`), retry: false });
export const useMembers = (id: string) => useQuery({ queryKey: qk.members(id), queryFn: () => api.get<Member[]>(`/projects/${id}/members`), enabled: !!id });
export const useInvites = (id: string, enabled: boolean) => useQuery({ queryKey: qk.invites(id), queryFn: () => api.get<Invite[]>(`/projects/${id}/invites`), enabled });
export const useAudit = (id: string, limit = 100) => useQuery({ queryKey: qk.audit(id), queryFn: () => api.get<AuditEntry[]>(`/projects/${id}/audit?limit=${limit}`), enabled: !!id });
export const useGithub = (id: string) =>
  useQuery({
    queryKey: qk.github(id),
    queryFn: async () => {
      try {
        return await api.get<GithubStatus>(`/projects/${id}/github`);
      } catch (e) {
        if (isUnsupported(e)) return null;
        throw e;
      }
    },
    retry: false,
  });
export const useFiles = (thread: string, path: string, enabled = true) =>
  useQuery({ queryKey: qk.files(thread, path), queryFn: () => api.get<WorkspaceFile[]>(`/threads/${thread}/files?path=${encodeURIComponent(path)}`), enabled, retry: false });

const memPath = (project?: string, suffix = "") => `/memory${suffix}${project ? `?project=${encodeURIComponent(project)}` : ""}`;
export const useMemory = (project?: string) => useQuery({ queryKey: qk.memory(project), queryFn: () => api.get<MemoryList>(memPath(project)) });

export function errMessage(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}
export const toastError = (e: unknown) => {
  if (e instanceof ApiError && e.status === 401) return;
  toast.error(errMessage(e));
};

/** Mutation helper: toasts failures, invalidates the given keys on success. */
function useAction<V, R>(fn: (v: V) => Promise<R>, invalidate: (v: V, r: R) => readonly (readonly unknown[])[], success?: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: fn,
    onSuccess: (r, v) => {
      for (const k of invalidate(v, r)) void qc.invalidateQueries({ queryKey: k });
      if (success) toast.success(success);
    },
    onError: toastError,
  });
}

export const useCreateProject = () =>
  useAction((b: { name: string; description?: string; repoUrl?: string; defaultBranch?: string }) => api.post<Project>("/projects", b), () => [qk.me, qk.overview], "Project created");
export const useUpdateProject = (id: string) =>
  useAction((b: Partial<Pick<Project, "name" | "description" | "repoUrl" | "defaultBranch">>) => api.patch<ProjectWithRole>(`/projects/${id}`, b), () => [qk.project(id), qk.me, qk.overview], "Saved");
export const useDeleteProject = () => useAction((id: string) => api.del(`/projects/${id}`), () => [qk.me, qk.overview], "Project deleted");
export const useSetMember = (pid: string) => useAction((v: { login: string; role: Role }) => api.put(`/projects/${pid}/members/${encodeURIComponent(v.login)}`, { role: v.role }), () => [qk.members(pid)]);
export const useRemoveMember = (pid: string) => useAction((login: string) => api.del(`/projects/${pid}/members/${encodeURIComponent(login)}`), () => [qk.members(pid), qk.me, qk.overview]);
export const useInvite = (pid: string) =>
  useAction((v: { email: string; role: Role }) => api.post<{ added?: boolean } | Invite>(`/projects/${pid}/invites`, v), () => [qk.members(pid), qk.invites(pid)]);
export const useRevokeInvite = (pid: string) => useAction((email: string) => api.del(`/projects/${pid}/invites?email=${encodeURIComponent(email)}`), () => [qk.invites(pid)]);
export const useRecheckGithub = (pid: string) => useAction(() => api.post<GithubStatus>(`/projects/${pid}/github/connect`), () => [qk.github(pid), qk.project(pid)]);

export const useCreateThread = () => useAction((b: { title: string; projectId?: string }) => api.post<Thread>("/threads", b), () => [qk.overview]);
export const useRenameThread = (id: string) => useAction((title: string) => api.patch<Thread>(`/threads/${id}`, { title }), () => [qk.thread(id), qk.overview]);
export const useDeleteThread = () => useAction((id: string) => api.del(`/threads/${id}`), () => [qk.overview], "Session deleted");

export const useSaveMemory = (project?: string) =>
  useAction((v: { id?: string; input: MemoryInput }) => (v.id ? api.patch<MemoryEntry>(memPath(project, `/${v.id}`), v.input) : api.post<MemoryEntry>(memPath(project), v.input)), () => [qk.memory(project)]);
export const useDeleteMemory = (project?: string) => useAction((id: string) => api.del(memPath(project, `/${id}`)), () => [qk.memory(project)]);
export const useDecideMemory = (project?: string) =>
  useAction((v: { id: string; action: "confirm" | "reject" }) => api.post(memPath(project, `/${v.id}/${v.action}`)), () => [qk.memory(project)]);

export const useSupervisorSettings = () =>
  useQuery({
    queryKey: ["supervisor", "settings"],
    queryFn: async () => {
      try {
        return await api.get<import("./types").SupervisorSettings>("/supervisor/settings");
      } catch (e) {
        if (isUnsupported(e)) return null;
        throw e;
      }
    },
    retry: false,
  });
export const useSaveSupervisorSettings = () =>
  useAction((v: import("./types").SupervisorSettings) => api.put<import("./types").SupervisorSettings>("/supervisor/settings", v), () => [["supervisor", "settings"]]);
