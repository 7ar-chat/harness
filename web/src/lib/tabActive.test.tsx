import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, render, renderHook, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, describe, expect, it } from "vitest";
import { mockFetch } from "@/test/fetch";
import { api } from "@/api/client";
import { TabActiveProvider, useLive, useLivePoll, useTabActive } from "./tabActive";

const client = () => new QueryClient({ defaultOptions: { queries: { retry: false } } });
const wrapper = (active: boolean) => ({ children }: { children: ReactNode }) => (
  <QueryClientProvider client={client()}>
    <TabActiveProvider active={active}>{children}</TabActiveProvider>
  </QueryClientProvider>
);

afterEach(() => Object.defineProperty(document, "visibilityState", { value: "visible", configurable: true }));

describe("tab activity", () => {
  it("counts as active outside a tab host", () => {
    const { result } = renderHook(() => useTabActive());
    expect(result.current).toBe(true);
  });

  it("turns polling off while the tab is hidden, and back on when it is shown", () => {
    const hidden = renderHook(() => useLivePoll(1000), { wrapper: wrapper(false) });
    const shown = renderHook(() => useLivePoll(1000), { wrapper: wrapper(true) });
    expect(hidden.result.current).toBe(false);
    expect(shown.result.current).toBe(1000);
  });

  it("also stops while the browser tab is in the background", () => {
    const { result } = renderHook(() => useLive(), { wrapper: wrapper(true) });
    expect(result.current).toBe(true);
    act(() => {
      Object.defineProperty(document, "visibilityState", { value: "hidden", configurable: true });
      document.dispatchEvent(new Event("visibilitychange"));
    });
    expect(result.current).toBe(false);
  });

  it("a hidden tab does not poll, and refetches its stale data when it is shown", async () => {
    const f = mockFetch({ "GET /api/thing": { n: 1 } });
    const qc = client();
    function Thing() {
      const interval = useLivePoll(30);
      const q = useQuery({ queryKey: ["thing"], queryFn: () => api.get<{ n: number }>("/thing"), refetchInterval: interval, staleTime: 0 });
      return <span>{q.data ? `n=${q.data.n}` : "loading"}</span>;
    }
    const ui = (active: boolean) => (
      <QueryClientProvider client={qc}>
        <TabActiveProvider active={active}>
          <Thing />
        </TabActiveProvider>
      </QueryClientProvider>
    );
    const { rerender } = render(ui(true));
    await screen.findByText("n=1");
    rerender(ui(false));
    const before = f.find("GET", "/api/thing").length;
    await new Promise((r) => setTimeout(r, 200));
    expect(f.find("GET", "/api/thing").length).toBe(before); // no polling while hidden
    rerender(ui(true));
    await waitFor(() => expect(f.find("GET", "/api/thing").length).toBeGreaterThan(before)); // refetched on return
  });
});
