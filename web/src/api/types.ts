// Wire types of the harness web API (see hosts/web and hosts/client in the Go repo).

export type Role = "owner" | "member" | "viewer";
export type ThreadStatus = "idle" | "queued" | "running" | "waiting_approval" | "waiting_input" | "failed";
export type ThreadKind = "chat" | "supervisor";

export interface User {
  login: string;
  name?: string;
  email?: string;
  avatarUrl?: string;
}

export interface Project {
  id: string;
  name: string;
  description?: string;
  repoUrl?: string;
  defaultBranch?: string;
  githubInstallation?: number;
  createdBy: string;
  createdAt: string;
  updatedAt: string;
}
export interface ProjectWithRole extends Project {
  role: Role;
}

export interface Me {
  user: User;
  projects: ProjectWithRole[];
}

export interface Member {
  projectId: string;
  login: string;
  role: Role;
  name?: string;
  avatarUrl?: string;
  email?: string;
  addedAt: string;
}
export interface Invite {
  projectId: string;
  email: string;
  role: Role;
  invitedBy: string;
  createdAt: string;
}
export interface AuditEntry {
  id: number;
  projectId?: string;
  threadId?: string;
  actor: string;
  onBehalfOf?: string;
  action: string;
  detail?: Record<string, unknown>;
  at: string;
}

export interface ThreadSummary {
  id: string;
  title: string;
  projectId?: string;
  createdBy?: string;
  kind?: ThreadKind;
  status: ThreadStatus;
  runId?: string;
  pendingTool?: string;
  pendingQuestion?: string;
  createdAt: string;
  updatedAt: string;
}
export interface OverviewProject extends ProjectWithRole {
  threads: ThreadSummary[];
}
export interface Overview {
  personal: ThreadSummary[];
  projects: OverviewProject[];
}

export interface Author {
  login: string;
  kind?: "user" | "supervisor" | "system";
  onBehalfOf?: string;
  name?: string;
  avatarUrl?: string;
}
export interface ToolCall {
  id: string;
  type: string;
  function: { name: string; arguments: string };
}
export interface Message {
  id: string;
  runId?: string;
  role: "user" | "assistant" | "tool" | "system";
  content: string;
  toolCalls?: ToolCall[];
  toolCallId?: string;
  error?: string;
  metadata?: Record<string, unknown>;
  createdAt: string;
  author?: Author;
}
export interface ActiveRun {
  id: string;
  status: string;
}
export interface PendingApproval {
  id: string;
  toolName?: string;
  toolInputPreview?: string;
  runId?: string;
  toolCallId?: string;
  [k: string]: unknown;
}
export interface QuestionOption {
  label: string;
  description?: string;
}
export interface PendingQuestion {
  interruptId: string;
  runId?: string;
  toolCallId?: string;
  question: string;
  options?: QuestionOption[];
  allowFreeText?: boolean;
}
export interface PlanItem {
  id: string;
  title: string;
  status: string;
}
export interface Thread {
  id: string;
  title: string;
  pinned?: boolean;
  messages: Message[];
  activeRun: ActiveRun | null;
  pendingApproval: PendingApproval | null;
  pendingQuestion: PendingQuestion | null;
  projectId?: string;
  createdBy?: string;
  kind?: ThreadKind;
  role?: Role;
  status: ThreadStatus;
  branch?: string;
  workspace?: Record<string, unknown>;
  plan?: PlanItem[];
  createdAt: string;
  updatedAt: string;
}

export interface Run {
  id: string;
  threadId: string;
  status: string;
}

export interface Attachment {
  name: string;
  mime: string;
  data: string;
}
export interface SendOptions {
  message: string;
  model?: string;
  reasoningEffort?: string;
  interactive?: boolean;
  mode?: "build" | "plan";
  sandbox?: "auto" | "on" | "off";
  attachments?: Attachment[];
}
export interface ResumeBody {
  interruptId: string;
  reason?: string;
  decision?: "approve" | "deny";
  always?: boolean;
  text?: string;
  model?: string;
  interactive?: boolean;
  mode?: "build" | "plan";
}

export interface ModelInfo {
  provider?: string;
  contextWindow?: number;
  inputPerToken?: number;
  outputPerToken?: number;
  priced?: boolean;
  vision?: boolean;
  reasoning?: boolean;
}
export interface Models {
  models: string[];
  default: string;
  info?: Record<string, ModelInfo>;
}
export interface Command {
  name: string;
  description?: string;
  source?: string;
  argHint?: string;
}
export interface WorkspaceFile {
  name: string;
  path: string;
  type: "file" | "directory";
  size: number;
  modifiedAt?: string;
}

export interface MemoryEntry {
  id: string;
  path?: string;
  title: string;
  content: string;
  tags?: string[];
  status: "active" | "pending";
  proposal?: { title?: string; content?: string; tags?: string[] } | null;
  updated_at?: string;
  source?: string;
  metadata?: Record<string, unknown>;
}
export interface MemoryList {
  entries: MemoryEntry[];
  enabled: boolean;
}
export interface MemoryInput {
  title: string;
  content: string;
  tags: string[];
}

export interface GithubStatus {
  configured: boolean;
  connected: boolean;
  installUrl?: string;
  repo?: string;
  message?: string;
}

export type FeedType =
  | "thread.created"
  | "thread.updated"
  | "thread.deleted"
  | "run.started"
  | "run.finished"
  | "message.sent"
  | "member.changed"
  | "project.changed"
  | "memory.changed";
export interface FeedEvent {
  type: FeedType;
  projectId?: string;
  threadId?: string;
  runId?: string;
  actor?: string;
  status?: string;
  title?: string;
  at: string;
}

export interface SupervisorSettings {
  watch: boolean;
  stuckMinutes: number;
}
