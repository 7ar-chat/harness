import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";
import type { QueueItem } from "@/api/types";
import { mockFetch } from "@/test/fetch";
import { QueueCard, useQueue } from "./queue";

const T = "2026-01-01T00:00:00Z";
const item = (id: string, text: string): QueueItem => ({ id, text, createdAt: T });

function Harness({ running = true }: { running?: boolean }) {
  const q = useQueue("t1");
  return <QueueCard items={q.items} running={running} readOnly={false} remove={q.remove} sendNow={q.sendNow} pending={q.pending} />;
}
const wrap = (ui: ReactNode) => {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
};

describe("QueueCard", () => {
  it("renders nothing for an empty queue", () => {
    const { container } = wrap(<QueueCard items={[]} running readOnly={false} remove={vi.fn()} sendNow={vi.fn()} pending={false} />);
    expect(container.innerHTML).toBe("");
  });

  it("lists the queued messages in order, with Steer now and Remove for each", async () => {
    const f = mockFetch({ "GET /api/threads/t1/queue": [item("q1", "second thing"), item("q2", "third thing")] });
    wrap(<Harness />);
    expect(await screen.findByText("2 messages queued")).toBeTruthy();
    const rows = screen.getAllByRole("listitem");
    expect(rows.map((r) => r.textContent)).toEqual([expect.stringContaining("second thing"), expect.stringContaining("third thing")]);
    expect(screen.getAllByRole("button", { name: /Steer now/ })).toHaveLength(2);
    expect(f.find("GET", "/api/threads/t1/queue")).toHaveLength(1);
  });

  it("Steer now posts the send route, Remove deletes the item", async () => {
    const f = mockFetch({
      "GET /api/threads/t1/queue": [item("q1", "a"), item("q2", "b")],
      "POST /api/threads/t1/queue/q1/send": { steered: true },
      "DELETE /api/threads/t1/queue/q2": new Response(null, { status: 204 }),
    });
    wrap(<Harness />);
    fireEvent.click(await screen.findByRole("button", { name: "Steer now: a" }));
    await waitFor(() => expect(f.find("POST", "/api/threads/t1/queue/q1/send")).toHaveLength(1));
    fireEvent.click(screen.getByRole("button", { name: "Remove: b" }));
    await waitFor(() => expect(f.find("DELETE", "/api/threads/t1/queue/q2")).toHaveLength(1));
  });

  it("offers Send now, and says why it waits, when no reply is running", async () => {
    mockFetch({ "GET /api/threads/t1/queue": [item("q1", "stuck")] });
    wrap(<Harness running={false} />);
    expect(await screen.findByRole("button", { name: "Send now: stuck" })).toBeTruthy();
    expect(screen.getByText(/did not complete/)).toBeTruthy();
  });
});
