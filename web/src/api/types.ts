// Wire types of the harness web API. The ones the Go servers define (hosts/client, core/store, product,
// features/memory) are generated into ./generated.ts by `make gen` (see cmd/gen-ts) and re-exported here, so
// the app imports every API type from this module. What is declared below is web-only: the shape of GET
// /threads/{id} the app relies on, and the bodies the app sends that the servers decode from unnamed structs.

import type { GithubRepo, Message, PlanItem, ProjectWithRole, Thread as ServerThread, User } from "./generated";

export * from "./generated";

/** A thread as GET /threads/{id} answers: its messages are present, and a server may add the workspace's branch and plan. */
export interface Thread extends ServerThread {
  messages: Message[];
  branch?: string;
  workspace?: Record<string, unknown>;
  plan?: PlanItem[];
}

export interface Me {
  user: User;
  projects: ProjectWithRole[];
}

/** A user the app knows only in part (a project member carries no display name or avatar flag). */
export type KnownUser = Pick<User, "login"> & Partial<User>;

export interface GithubRepos {
  repos: GithubRepo[];
  installUrl?: string;
}

/** The body the memory dialog sends to create or patch an entry (the handlers decode it from unnamed structs, so it has no generated type). */
export interface MemoryInput {
  title: string;
  content: string;
  tags: string[];
}
