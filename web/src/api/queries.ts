import { queryOptions, useMutation, useQueries, useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useMemo } from "react";
import { toast } from "sonner";
import { api, ApiError, isUnsupported, putBytes } from "./client";
import { isNewerVisit, loadLocalSeen, saveLocalSeen } from "@/lib/unread";
import type {
  AuditEntry, Connector, GithubRepos, KnownUser, User, Command, GithubStatus, Invite, Me, Member, MemoryEntry, MemoryInput, MemoryList, MemoryStats, ModelList, Overview,
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
  files: (thread: string, path: string) => ["files", thread, path] as const,
  seen: ["seen"] as const,
};

export const meQuery = queryOptions({ queryKey: qk.me, queryFn: () => api.get<Me>("/me"), staleTime: 60_000, retry: (n, e) => !(e instanceof ApiError && e.status < 500) && n < 2 });
export const overviewQuery = queryOptions({ queryKey: qk.overview, queryFn: () => api.get<Overview>("/overview"), staleTime: 30_000 });
export const modelsQuery = queryOptions({ queryKey: qk.models, queryFn: () => api.get<ModelList>("/models"), staleTime: 300_000, retry: 1 });
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

/** Last visit per thread. `server` is false when the server has no /api/seen: the map then lives in local storage. */
export interface SeenState {
  server: boolean;
  seen: Record<string, string>;
}
export const seenQuery = queryOptions({
  queryKey: qk.seen,
  queryFn: async (): Promise<SeenState> => {
    try {
      return { server: true, seen: await api.get<Record<string, string>>("/seen") };
    } catch (e) {
      if (isUnsupported(e)) return { server: false, seen: loadLocalSeen() };
      throw e;
    }
  },
  retry: false,
  staleTime: 60_000,
});
const NO_SEEN: Record<string, string> = {};
/** The caller's last visit per thread; empty until loaded. */
export const useSeen = () => useQuery(seenQuery).data?.seen ?? NO_SEEN;

/**
 * Records a visit to a thread: the cached map updates at once (never backwards), then the server learns
 * about it. A failed save (e.g. a thread we cannot see) is undone by refetching the server's answer.
 */
export function useMarkSeen() {
  const qc = useQueryClient();
  const { mutate } = useMutation({
    mutationFn: (v: { threadId: string; at: string }) => api.post("/seen", v),
    onError: () => void qc.invalidateQueries({ queryKey: qk.seen }),
  });
  return useCallback(
    async (threadId: string, at: string = new Date().toISOString()) => {
      let s: SeenState;
      try {
        s = await qc.ensureQueryData(seenQuery);
      } catch {
        return; // the map could not be loaded; the next visit tries again
      }
      if (!isNewerVisit(s.seen, threadId, at)) return;
      const seen = { ...s.seen, [threadId]: at };
      void qc.cancelQueries({ queryKey: qk.seen }); // a refetch that started before this visit must not overwrite it
      qc.setQueryData<SeenState>(qk.seen, { ...s, seen });
      if (s.server) mutate({ threadId, at });
      else saveLocalSeen(seen);
    },
    [qc, mutate],
  );
}

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

export interface MemScope {
  project?: string;
  /** The user's private layer inside the project. */
  mine?: boolean;
}
const memQuery = (sc: MemScope) => (sc.project ? `?project=${encodeURIComponent(sc.project)}${sc.mine ? "&mine=1" : ""}` : "");
const memPath = (sc: MemScope, suffix = "") => `/memory${suffix}${memQuery(sc)}`;
const memKey = (sc: MemScope, extra?: string) => ["memory", sc.project ?? "user", sc.mine ? "mine" : "shared", ...(extra ? [extra] : [])] as const;
export const useMemory = (sc: MemScope) => useQuery({ queryKey: memKey(sc), queryFn: () => api.get<MemoryList>(memPath(sc)) });
export const useMemoryStats = (sc: MemScope, enabled = true) =>
  useQuery({
    queryKey: memKey(sc, "stats"),
    queryFn: async () => {
      try {
        return await api.get<MemoryStats>(memPath(sc, "/stats"));
      } catch (e) {
        if (isUnsupported(e)) return null;
        throw e;
      }
    },
    enabled,
    retry: false,
    staleTime: 30_000,
  });

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

const memInvalidate = () => [["memory"]] as const;
export const useSaveMemory = (sc: MemScope) =>
  useAction((v: { id?: string; input: Partial<MemoryInput> & { pinned?: boolean } }) => (v.id ? api.patch<MemoryEntry>(memPath(sc, `/${v.id}`), v.input) : api.post<MemoryEntry>(memPath(sc), v.input)), memInvalidate);
export const useDeleteMemory = (sc: MemScope) => useAction((id: string) => api.del(memPath(sc, `/${id}`)), memInvalidate);
export const useDecideMemory = (sc: MemScope) =>
  useAction((v: { id: string; action: "confirm" | "reject" }) => api.post(memPath(sc, `/${v.id}/${v.action}`)), memInvalidate);
/** Keeps or discards several entries; reports how many succeeded. */
export const useBulkDecide = (sc: MemScope) =>
  useAction(
    async (v: { ids: string[]; action: "confirm" | "reject" }) => {
      const rs = await Promise.allSettled(v.ids.map((id) => api.post(memPath(sc, `/${id}/${v.action}`))));
      return rs.filter((r) => r.status === "fulfilled").length;
    },
    memInvalidate,
  );
export const useImportAgents = (pid: string) => useAction(() => api.post<MemoryEntry>(`/projects/${pid}/memory/import-agents`), memInvalidate);

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

// ---- profile and connectors ----
export const useConnectors = (refetchInterval?: number) =>
  useQuery({
    queryKey: ["connectors"],
    queryFn: async () => {
      try {
        return await api.get<Connector[]>("/connectors");
      } catch (e) {
        if (isUnsupported(e)) return [];
        throw e;
      }
    },
    refetchInterval,
    retry: false,
  });

/** After a profile change, user objects appear in me, messages and members: refresh them all. */
const profileKeys = () => [qk.me, ["thread"], ["project"], qk.overview] as const;
export const useSaveProfile = () => useAction((displayName: string) => api.patch<User>("/me/profile", { displayName }), profileKeys, "Profile saved");
export const useUploadAvatar = () => useAction((blob: Blob) => putBytes<User>("/me/avatar", blob), profileKeys, "Avatar updated");
export const useRemoveAvatar = () => useAction(() => api.del<User>("/me/avatar"), profileKeys, "Avatar removed");
export const useDisconnect = (kind: "github" | "telegram") => useAction(() => api.del(`/connectors/${kind}`), () => [["connectors"]], "Disconnected");
export const useTelegramLink = () => useAction(() => api.post<{ code: string; url: string; expiresAt: string }>("/connectors/telegram/link"), () => [["connectors"]]);
export const useTelegramTest = () => useAction(() => api.post("/connectors/telegram/test"), () => [], "Test message sent");
export const useGithubRepos = (q: string, enabled: boolean) =>
  useQuery({ queryKey: ["github-repos", q], queryFn: () => api.get<GithubRepos>(`/connectors/github/repos?q=${encodeURIComponent(q)}`), enabled, retry: false, staleTime: 15_000 });
export const useImportProject = () => useAction((repo: string) => api.post<Project>("/projects/import", { repo }), () => [qk.me, qk.overview, ["github-repos"]], "Project imported");

/** Every user we know of (me and the members of my projects) by login: name and avatar. */
export function useUserDirectory(): Map<string, KnownUser> {
  const me = useMe().data;
  const projects = me?.projects ?? [];
  const members = useQueries({
    queries: projects.map((p) => ({ queryKey: qk.members(p.id), queryFn: () => api.get<Member[]>(`/projects/${p.id}/members`), staleTime: 120_000 })),
    combine: (rs) => rs.flatMap((r) => r.data ?? []),
  });
  return useMemo(() => {
    const m = new Map<string, KnownUser>();
    for (const x of members) m.set(x.login, { login: x.login, name: x.name, avatarUrl: x.avatarUrl, email: x.email });
    if (me) m.set(me.user.login, me.user);
    return m;
  }, [members, me]);
}
