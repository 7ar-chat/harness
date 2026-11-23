import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { mockFetch } from "@/test/fetch";
import { EnvironmentControl } from "./environment";

const navigate = vi.fn();
vi.mock("@tanstack/react-router", () => ({ useNavigate: () => navigate }));

const list = (selectable: boolean) => ({
  environments: [
    { name: "work", kind: "opensandbox", description: "d", egress: "allowlist: a.com", default: true },
    { name: "coding", kind: "opensandbox", description: "d", egress: "allowlist: a.com" },
  ],
  default: "work",
  selectable,
});
const show = () => {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <EnvironmentControl threadId="t1" projectId="p1" />
    </QueryClientProvider>,
  );
};

describe("EnvironmentControl", () => {
  it("is hidden when the server does not offer a choice", async () => {
    const f = mockFetch({ "GET /api/environments": list(false) });
    const { container } = show();
    await waitFor(() => expect(f.find("GET", "/api/environments")).toHaveLength(1));
    expect(container.innerHTML).toBe("");
    expect(f.find("GET", "/api/threads/t1/environment")).toHaveLength(0);
  });

  it("offers the environments and records the choice while the thread has no workspace", async () => {
    const f = mockFetch({
      "GET /api/environments": list(true),
      "GET /api/threads/t1/environment": { environment: "", frozen: false },
      "PUT /api/threads/t1/environment": { environment: "coding", frozen: false },
    });
    show();
    const select = await screen.findByRole("combobox", { name: "Runtime environment" });
    expect((select as HTMLSelectElement).value).toBe("work"); // the default shows when nothing was chosen
    expect(screen.getByText(/egress: allowlist: a.com/)).toBeTruthy();
    fireEvent.change(select, { target: { value: "coding" } });
    await waitFor(() => expect(f.find("PUT", "/api/threads/t1/environment")[0]?.body).toEqual({ environment: "coding" }));
    await waitFor(() => expect((select as HTMLSelectElement).value).toBe("coding"));
  });

  it("is frozen once the thread has a workspace: another choice offers a new session instead", async () => {
    const f = mockFetch({
      "GET /api/environments": list(true),
      "GET /api/threads/t1/environment": { environment: "work", frozen: true },
      "POST /api/threads": { id: "t2" },
      "PUT /api/threads/t2/environment": { environment: "coding", frozen: false },
    });
    show();
    const select = await screen.findByRole("combobox", { name: "Runtime environment" });
    expect(screen.getByText(/Fixed once the session has a workspace/)).toBeTruthy();
    fireEvent.change(select, { target: { value: "coding" } });
    expect(f.find("PUT", "/api/threads/t1/environment")).toHaveLength(0); // never changes the frozen thread
    fireEvent.click(screen.getByRole("button", { name: "Open a new session in coding" }));
    await waitFor(() => expect(navigate).toHaveBeenCalledWith({ to: "/t/$threadId", params: { threadId: "t2" } }));
    expect(f.find("POST", "/api/threads")[0]?.body).toEqual({ title: "", projectId: "p1", environment: "coding" });
    expect(f.find("PUT", "/api/threads/t2/environment")[0]?.body).toEqual({ environment: "coding" });
  });
});
