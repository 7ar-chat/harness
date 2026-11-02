import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { mockFetch, reply } from "@/test/fetch";
import { overview, shellRoutes } from "@/test/fixtures";
import { memoryEntry, memoryList, memoryStats } from "@/test/memory";
import { renderApp } from "@/test/render";

const routes = (entries = [memoryEntry("m1")], extra: Record<string, unknown> = {}) => ({
  ...shellRoutes(overview()),
  "GET /api/memory/stats": memoryStats({ active: entries.length }),
  "GET /api/memory": memoryList(entries),
  ...extra,
});

describe("MemoryPage", () => {
  it("lists the entries grouped by kind, with pinned ones first and the stats strip", async () => {
    // Arrange
    mockFetch(
      routes(
        [
          memoryEntry("a", { title: "Deploys go through staging", path: "memories/semantic/decisions/a.md", tags: ["ops"] }),
          memoryEntry("b", { title: "Prefers terse answers", path: "memories/semantic/preferences/b.md", metadata: { pinned: true } }),
        ],
        { "GET /api/memory/stats": memoryStats({ active: 2, pinned: 1, pending: 0, byKind: { decisions: 1, preferences: 1 } }) },
      ),
    );
    // Act
    await renderApp("/memory");
    // Assert
    expect(await screen.findByRole("heading", { name: "Deploys go through staging" })).toBeTruthy();
    const pinned = screen.getByRole("heading", { name: /Pinned \(1\)/ }).closest("section")!;
    expect(within(pinned).getByText("Prefers terse answers")).toBeTruthy();
    expect(screen.getByRole("region", { name: "decisions memories" })).toBeTruthy();
    expect(screen.getByText("ops")).toBeTruthy();
    expect(await screen.findByText("active")).toBeTruthy();
    expect(screen.getByLabelText("By kind: decisions 1, preferences 1")).toBeTruthy();
  });

  it("explains an empty memory", async () => {
    // Arrange
    mockFetch(routes([]));
    // Act
    await renderApp("/memory");
    // Assert
    expect(await screen.findByText("No memory yet")).toBeTruthy();
  });

  it("says so when memory is not enabled on the server", async () => {
    // Arrange
    mockFetch(routes([], { "GET /api/memory": memoryList([], false) }));
    // Act
    await renderApp("/memory");
    // Assert
    expect(await screen.findByText("Memory is not enabled on this server")).toBeTruthy();
  });

  it("shows the server error with a retry that refetches", async () => {
    // Arrange
    let fail = true;
    mockFetch(routes([], { "GET /api/memory": () => (fail ? reply(500, { error: { code: "internal", message: "store is down" } }) : memoryList([memoryEntry("m1", { title: "Back again" })])) }));
    await renderApp("/memory");
    expect(await screen.findByText("store is down")).toBeTruthy();
    // Act
    fail = false;
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    // Assert
    expect(await screen.findByRole("heading", { name: "Back again" })).toBeTruthy();
  });

  it("pinning an entry PATCHes it with pinned:true, and unpinning sends false", async () => {
    // Arrange
    const f = mockFetch(
      routes([memoryEntry("m1", { title: "Plain" }), memoryEntry("m2", { title: "Already pinned", metadata: { pinned: true } })], {
        "PATCH /api/memory/m1": memoryEntry("m1"),
        "PATCH /api/memory/m2": memoryEntry("m2"),
      }),
    );
    await renderApp("/memory");
    // Act
    fireEvent.click(await screen.findByRole("button", { name: "Pin Plain" }));
    fireEvent.click(screen.getByRole("button", { name: "Unpin Already pinned" }));
    // Assert
    await waitFor(() => expect(f.find("PATCH", "/api/memory/m1")).toHaveLength(1));
    expect(f.find("PATCH", "/api/memory/m1")[0]?.body).toEqual({ pinned: true });
    await waitFor(() => expect(f.find("PATCH", "/api/memory/m2")).toHaveLength(1));
    expect(f.find("PATCH", "/api/memory/m2")[0]?.body).toEqual({ pinned: false });
  });

  it("keeping a suggestion POSTs confirm and discarding POSTs reject, then the list is reloaded", async () => {
    // Arrange
    let lists = 0;
    const f = mockFetch(
      routes([], {
        "GET /api/memory": () => (++lists === 1 ? memoryList([memoryEntry("p1", { status: "pending", title: "CI is slow" }), memoryEntry("p2", { status: "pending", title: "Use gofmt" })]) : memoryList([])),
        "POST /api/memory/p1/confirm": memoryEntry("p1"),
        "POST /api/memory/p2/reject": reply(204),
      }),
    );
    await renderApp("/memory");
    // Act
    const keep = await screen.findByRole("listitem", { name: "Review: CI is slow" });
    fireEvent.click(within(keep).getByRole("button", { name: /Keep/ }));
    fireEvent.click(within(screen.getByRole("listitem", { name: "Review: Use gofmt" })).getByRole("button", { name: /Discard/ }));
    // Assert
    await waitFor(() => expect(f.find("POST", "/api/memory/p1/confirm")).toHaveLength(1));
    await waitFor(() => expect(f.find("POST", "/api/memory/p2/reject")).toHaveLength(1));
    await waitFor(() => expect(lists).toBeGreaterThan(1));
  });

  it("shows what an agent proposes to change next to the current text", async () => {
    // Arrange
    const proposal = { path: "p", title: "Deploys", content: "Deploy on Mondays", tags: [], importance: null, metadata: {}, content_hash: "", proposed_at: "2026-01-01T00:00:00Z" };
    mockFetch(routes([memoryEntry("m1", { title: "Deploys", content: "Deploy on Fridays", proposal })]));
    // Act
    await renderApp("/memory");
    // Assert
    const card = await screen.findByRole("listitem", { name: "Review: Deploys" });
    expect(within(card).getByText("Suggested change")).toBeTruthy();
    expect(within(card).getByText("Deploy on Fridays")).toBeTruthy();
    expect(within(card).getByText("Deploy on Mondays")).toBeTruthy();
  });

  it("deleting asks first, then DELETEs the entry", async () => {
    // Arrange
    const f = mockFetch(routes([memoryEntry("m1", { title: "Old note" })], { "DELETE /api/memory/m1": reply(204) }));
    await renderApp("/memory");
    // Act
    fireEvent.click(await screen.findByRole("button", { name: "Delete Old note" }));
    expect(f.find("DELETE", "/api/memory/")).toHaveLength(0);
    fireEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Delete" }));
    // Assert
    await waitFor(() => expect(f.find("DELETE", "/api/memory/m1")).toHaveLength(1));
  });

  it("adds an entry through the dialog with the typed fields", async () => {
    // Arrange
    const f = mockFetch(routes([], { "POST /api/memory": memoryEntry("n1") }));
    await renderApp("/memory");
    fireEvent.click(await screen.findByRole("button", { name: /Add/ }));
    // Act
    fireEvent.change(await screen.findByLabelText("Title"), { target: { value: " Style " } });
    fireEvent.change(screen.getByLabelText("Content"), { target: { value: "Terse answers." } });
    fireEvent.change(screen.getByLabelText(/^Tags/), { target: { value: "style, ops" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    // Assert
    await waitFor(() => expect(f.find("POST", "/api/memory")).toHaveLength(1));
    expect(f.find("POST", "/api/memory")[0]?.body).toEqual({ title: "Style", content: "Terse answers.", tags: ["style", "ops"], pinned: false });
  });
});
