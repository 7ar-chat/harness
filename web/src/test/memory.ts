import type { MemoryEntry, MemoryList, MemoryStats } from "@/api/types";

const T = "2026-01-01T00:00:00Z";

export const memoryEntry = (id: string, extra: Partial<MemoryEntry> = {}): MemoryEntry => ({
  id,
  content: `Content of ${id}`,
  created_at: T,
  embedding_updated_at: null,
  importance: null,
  metadata: {},
  path: `memories/semantic/facts/${id}.md`,
  repository: "default",
  scope: "personal",
  source: "user",
  status: "active",
  tags: [],
  title: `Entry ${id}`,
  updated_at: new Date().toISOString(),
  ...extra,
});

export const memoryList = (entries: MemoryEntry[] = [], enabled = true): MemoryList => ({ entries, enabled });

export const memoryStats = (extra: Partial<MemoryStats> = {}): MemoryStats => ({
  active: 0,
  byKind: {},
  embeddings: { enabled: false },
  pending: 0,
  pinned: 0,
  service: { recallRuns: 0, recallItems: 0, recallChars: 0, recallPinned: 0, searches: 0, searchHits: 0, extractRuns: 0, extractProposed: 0, extractSkipped: 0, extractErrors: 0, embedded: 0, embedErrors: 0 },
  ...extra,
});
