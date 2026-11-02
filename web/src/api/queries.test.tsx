import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { toast } from "sonner";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { mockFetch, reply } from "@/test/fetch";
import { ApiError } from "./client";
import { errMessage, qk, supervisorQuery, toastError, useCommands, useCreateProject, useGithub, useMemory, useMarkSeen, useMemoryStats, useSaveMemory, useSeen, useUserDirectory } from "./queries";

function wrapper() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const W = ({ children }: { children: ReactNode }) => <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  return { qc, W };
}

describe("toastError", () => {
  it("shows the message, but stays silent for 401 (the login redirect explains itself)", () => {
    const err = vi.spyOn(toast, "error").mockImplementation(() => "");
    toastError(new ApiError(401, "unauthenticated", "Authentication required."));
    expect(err).not.toHaveBeenCalled();
    toastError(new ApiError(500, "error", "Boom"));
    toastError("plain string");
    expect(err.mock.calls).toEqual([["Boom"], ["plain string"]]);
    expect(errMessage(new Error("e"))).toBe("e");
  });
});

describe("optional-feature queries", () => {
  it("turn 404/501 into an empty answer, but surface other failures", async () => {
    // Arrange
    mockFetch({
      "GET /api/commands": reply(501, { error: { code: "unsupported", message: "no" } }),
      "GET /api/projects/p1/github": reply(404, { error: { code: "nf", message: "no" } }),
      "GET /api/memory/stats": reply(404),
      "GET /api/supervisor": reply(501),
    });
    const { W, qc } = wrapper();
    // Act
    const commands = renderHook(() => useCommands(), { wrapper: W });
    const github = renderHook(() => useGithub("p1"), { wrapper: W });
    const stats = renderHook(() => useMemoryStats({}), { wrapper: W });
    // Assert
    await waitFor(() => expect(commands.result.current.data).toEqual([]));
    await waitFor(() => expect(github.result.current.data).toBeNull());
    await waitFor(() => expect(stats.result.current.data).toBeNull());
    await expect(qc.fetchQuery(supervisorQuery)).resolves.toBeNull();
  });

  it("surface a server error instead of pretending the feature is missing", async () => {
    mockFetch({ "GET /api/commands": reply(500, { error: { code: "x", message: "db down" } }) });
    const { W } = wrapper();
    const { result } = renderHook(() => useCommands(), { wrapper: W });
    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(result.current.error).toMatchObject({ status: 500, message: "db down" });
  });
});

describe("memory scoping", () => {
  it("sends the project and private-layer flags, URL-encoded, and keeps the layers in separate cache entries", async () => {
    const f = mockFetch({ "GET /api/memory": { entries: [], enabled: true } });
    const { W } = wrapper();
    renderHook(() => useMemory({}), { wrapper: W });
    renderHook(() => useMemory({ project: "a b" }), { wrapper: W });
    renderHook(() => useMemory({ project: "a b", mine: true }), { wrapper: W });
    await waitFor(() => expect(f.calls).toHaveLength(3));
    expect(f.calls.map((c) => c.url).sort()).toEqual(["/api/memory", "/api/memory?project=a%20b", "/api/memory?project=a%20b&mine=1"].sort());
  });

  it("saving routes to POST for a new entry and PATCH for an existing one, then refreshes the memory lists", async () => {
    const f = mockFetch({ "POST /api/memory?project=p1": { id: "n" }, "PATCH /api/memory/m1?project=p1": { id: "m1" } });
    const { W, qc } = wrapper();
    const inv = vi.spyOn(qc, "invalidateQueries");
    const { result } = renderHook(() => useSaveMemory({ project: "p1" }), { wrapper: W });
    result.current.mutate({ input: { title: "t" } });
    await waitFor(() => expect(f.find("POST", "/api/memory")).toHaveLength(1));
    result.current.mutate({ id: "m1", input: { title: "t2" } });
    await waitFor(() => expect(f.find("PATCH", "/api/memory/m1")).toHaveLength(1));
    await waitFor(() => expect(inv).toHaveBeenCalledWith({ queryKey: ["memory"] }));
  });
});

describe("mutations", () => {
  it("creating a project refreshes me and the overview, and a failure toasts instead of invalidating", async () => {
    const ok = vi.spyOn(toast, "success").mockImplementation(() => "");
    const bad = vi.spyOn(toast, "error").mockImplementation(() => "");
    let fail = false;
    mockFetch({ "POST /api/projects": () => (fail ? reply(409, { error: { code: "exists", message: "Name taken." } }) : { id: "p9" }) });
    const { W, qc } = wrapper();
    const inv = vi.spyOn(qc, "invalidateQueries");
    const { result } = renderHook(() => useCreateProject(), { wrapper: W });
    result.current.mutate({ name: "x" });
    await waitFor(() => expect(ok).toHaveBeenCalledWith("Project created"));
    expect(inv.mock.calls.map((c) => c[0]?.queryKey)).toEqual([qk.me, qk.overview]);
    inv.mockClear();
    fail = true;
    result.current.mutate({ name: "x" });
    await waitFor(() => expect(bad).toHaveBeenCalledWith("Name taken."));
    expect(inv).not.toHaveBeenCalled();
  });
});

describe("useUserDirectory", () => {
  it("lists me and the members of my projects, with me winning on a clash", async () => {
    mockFetch({
      "GET /api/me": { user: { login: "alice", name: "Alice (me)" }, projects: [{ id: "p1", name: "P", role: "owner" }] },
      "GET /api/projects/p1/members": [
        { login: "alice", name: "Alice (member)", role: "owner" },
        { login: "bob", name: "Bob", avatarUrl: "/b.png", email: "b@x", role: "member" },
      ],
    });
    const { W } = wrapper();
    const { result } = renderHook(() => useUserDirectory(), { wrapper: W });
    await waitFor(() => expect(result.current.size).toBe(2));
    expect(result.current.get("alice")?.name).toBe("Alice (me)");
    expect(result.current.get("bob")).toEqual({ login: "bob", name: "Bob", avatarUrl: "/b.png", email: "b@x" });
  });
});

describe("seen state", () => {
  beforeEach(() => localStorage.clear());

  it("the server's map wins over whatever local storage holds", async () => {
    // Arrange
    localStorage.setItem("harness.seen", JSON.stringify({ t1: "2026-01-09T00:00:00Z", old: "2026-01-01T00:00:00Z" }));
    mockFetch({ "GET /api/seen": { t1: "2026-01-02T00:00:00Z" } });
    const { W } = wrapper();
    // Act
    const { result } = renderHook(() => useSeen(), { wrapper: W });
    // Assert
    await waitFor(() => expect(result.current).toEqual({ t1: "2026-01-02T00:00:00Z" }));
  });

  it("marking a thread seen updates the map before the server answers, then posts the visit", async () => {
    // Arrange
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    const f = mockFetch({
      "GET /api/seen": { t1: "2026-01-02T00:00:00Z" },
      "POST /api/seen": reply(204),
    });
    // mockFetch answers synchronously: hold the POST back until the test releases it.
    const answer = f.fn.getMockImplementation()!;
    f.fn.mockImplementation(async (input, init) => {
      if (init?.method === "POST") await gate;
      return answer(input, init);
    });
    const { W, qc } = wrapper();
    const { result } = renderHook(() => ({ seen: useSeen(), mark: useMarkSeen() }), { wrapper: W });
    await waitFor(() => expect(result.current.seen).toEqual({ t1: "2026-01-02T00:00:00Z" }));
    // Act
    await act(() => result.current.mark("t1", "2026-01-03T00:00:00Z"));
    // Assert: visible while the request is still in flight
    await waitFor(() => expect(result.current.seen).toEqual({ t1: "2026-01-03T00:00:00Z" }));
    await waitFor(() => expect(qc.isMutating()).toBe(1));
    release();
    await waitFor(() => expect(qc.isMutating()).toBe(0));
    expect(f.find("POST", "/api/seen")).toHaveLength(1);
    expect(f.find("POST", "/api/seen")[0]?.body).toEqual({ threadId: "t1", at: "2026-01-03T00:00:00Z" });
    expect(localStorage.getItem("harness.seen")).toBeNull();
  });

  it("does not post a visit that is not newer than the recorded one", async () => {
    const f = mockFetch({ "GET /api/seen": { t1: "2026-01-02T00:00:00Z" }, "POST /api/seen": reply(204) });
    const { W } = wrapper();
    const { result } = renderHook(() => ({ seen: useSeen(), mark: useMarkSeen() }), { wrapper: W });
    await waitFor(() => expect(result.current.seen).toEqual({ t1: "2026-01-02T00:00:00Z" }));
    await act(async () => {
      await result.current.mark("t1", "2026-01-01T00:00:00Z");
      await result.current.mark("t1", "2026-01-02T00:00:00Z");
    });
    expect(f.find("POST", "/api/seen")).toHaveLength(0);
    expect(result.current.seen).toEqual({ t1: "2026-01-02T00:00:00Z" });
  });

  it("marks a thread seen on a server that has not loaded the map yet, using the loaded map afterwards", async () => {
    const f = mockFetch({ "GET /api/seen": {}, "POST /api/seen": reply(204) });
    const { W } = wrapper();
    const { result } = renderHook(() => useMarkSeen(), { wrapper: W });
    await act(() => result.current("t9", "2026-01-03T00:00:00Z"));
    await waitFor(() => expect(f.find("POST", "/api/seen")).toHaveLength(1));
  });

  it("refetches the server's map when saving a visit fails, undoing the optimistic update", async () => {
    // Arrange
    let gets = 0;
    const f = mockFetch({
      "GET /api/seen": () => (++gets, { t1: "2026-01-02T00:00:00Z" }),
      "POST /api/seen": reply(404, { error: { code: "not_found", message: "no" } }),
    });
    const { W } = wrapper();
    const { result } = renderHook(() => ({ seen: useSeen(), mark: useMarkSeen() }), { wrapper: W });
    await waitFor(() => expect(result.current.seen).toEqual({ t1: "2026-01-02T00:00:00Z" }));
    // Act
    await act(() => result.current.mark("t1", "2026-01-03T00:00:00Z"));
    // Assert
    await waitFor(() => expect(gets).toBe(2));
    await waitFor(() => expect(result.current.seen).toEqual({ t1: "2026-01-02T00:00:00Z" }));
    expect(f.find("POST", "/api/seen")).toHaveLength(1);
  });

  it.each([404, 501])("falls back to local storage when the server answers %i", async (status) => {
    // Arrange
    localStorage.setItem("harness.seen", JSON.stringify({ a: "2026-01-05T00:00:00Z" }));
    const f = mockFetch({ "GET /api/seen": reply(status) });
    const { W } = wrapper();
    const { result } = renderHook(() => ({ seen: useSeen(), mark: useMarkSeen() }), { wrapper: W });
    await waitFor(() => expect(result.current.seen).toEqual({ a: "2026-01-05T00:00:00Z" }));
    // Act
    await act(async () => {
      await result.current.mark("b", "2026-01-06T00:00:00Z");
      await result.current.mark("a", "2026-01-01T00:00:00Z"); // older: ignored
    });
    // Assert
    await waitFor(() => expect(result.current.seen).toEqual({ a: "2026-01-05T00:00:00Z", b: "2026-01-06T00:00:00Z" }));
    expect(JSON.parse(localStorage.getItem("harness.seen") ?? "{}")).toEqual({ a: "2026-01-05T00:00:00Z", b: "2026-01-06T00:00:00Z" });
    expect(f.find("POST", "/api/seen")).toHaveLength(0);
  });

  it("surfaces a server error instead of treating it as single-user mode", async () => {
    mockFetch({ "GET /api/seen": reply(500, { error: { code: "x", message: "db down" } }) });
    const { W, qc } = wrapper();
    const { result } = renderHook(() => ({ seen: useSeen(), mark: useMarkSeen() }), { wrapper: W });
    await waitFor(() => expect(qc.getQueryState(qk.seen)?.status).toBe("error"));
    expect(result.current.seen).toEqual({});
    await act(() => result.current.mark("t1", "2026-01-03T00:00:00Z"));
    expect(localStorage.getItem("harness.seen")).toBeNull();
  });
});
