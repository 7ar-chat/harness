import type { Me, Overview, ProjectWithRole, ThreadSummary } from "@/api/types";

const T = "2026-01-01T00:00:00Z";

export const project = (id: string, extra: Partial<ProjectWithRole> = {}): ProjectWithRole => ({ id, name: `proj-${id}`, role: "owner", createdBy: "alice", createdAt: T, updatedAt: T, ...extra });
export const thread = (id: string, extra: Partial<ThreadSummary> = {}): ThreadSummary => ({ id, title: `Session ${id}`, kind: "chat", status: "idle", createdBy: "alice", createdAt: T, updatedAt: new Date().toISOString(), ...extra });
export const me = (extra: Partial<Me> = {}): Me => ({ user: { login: "alice", name: "Alice Doe", displayName: "", defaultName: "Alice Doe", email: "alice@example.com", avatarUrl: "", hasCustomAvatar: false }, projects: [], ...extra });
export const overview = (extra: Partial<Overview> = {}): Overview => ({ personal: [], projects: [], ...extra });

/** The calls every page makes through the app shell and feed. */
export const shellRoutes = (o: Overview = overview(), m: Me = me()) => ({
  "GET /api/me": m,
  "GET /api/overview": o,
  "GET /api/supervisor": { id: "sv", title: "Global agent", status: "idle", messages: [], activeRun: null, pendingApproval: null, pendingQuestion: null, createdAt: T, updatedAt: T },
});
