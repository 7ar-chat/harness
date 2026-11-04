import { render, screen } from "@testing-library/react";
import { QueryClientProvider } from "@tanstack/react-query";
import { describe, expect, it } from "vitest";
import { mockFetch } from "@/test/fetch";
import { newQueryClient } from "@/test/render";
import { PreviewPanel } from "./panels";

const view = (active = true) =>
  render(
    <QueryClientProvider client={newQueryClient()}>
      <PreviewPanel threadId="t1" active={active} />
    </QueryClientProvider>,
  );

describe("PreviewPanel", () => {
  it("frames the running app in a sandbox without same-origin access", async () => {
    mockFetch({ "GET /api/threads/t1/preview": { url: "/preview/t1.abc/", running: true, manifest: { run: "npm run dev", port: 5173 } } });
    view();
    const frame = (await screen.findByTitle("Preview")) as HTMLIFrameElement;
    expect(frame.getAttribute("src")).toBe("/preview/t1.abc/");
    const sandbox = frame.getAttribute("sandbox") ?? "";
    expect(sandbox).toContain("allow-scripts");
    expect(sandbox).not.toContain("allow-same-origin");
    expect(screen.getByRole("link", { name: "Open in a new tab" }).getAttribute("href")).toBe("/preview/t1.abc/");
  });

  it("tells the user to ask the assistant when nothing runs", async () => {
    mockFetch({ "GET /api/threads/t1/preview": { url: "/preview/t1.abc/", running: false, reason: "not started" } });
    view();
    expect(await screen.findByText("Nothing is running")).toBeTruthy();
    expect(screen.queryByTitle("Preview")).toBeNull();
  });

  it("does not look for the preview while its tab is closed", async () => {
    const m = mockFetch({});
    view(false);
    await screen.findByText("Looking for the preview…").catch(() => null);
    expect(m.find("GET", "/api/threads/t1/preview")).toHaveLength(0);
  });
});
