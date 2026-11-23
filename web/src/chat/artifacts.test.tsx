import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { Message } from "@/api/types";
import { mockFetch } from "@/test/fetch";
import { ArtifactCard, ArtifactContext, artifactKind, parseCsv, PublishedList, publishedFromResult, type PublishedItem } from "./artifacts";
import { toThreadMessage } from "./convert";
import { messagesFromThread } from "./model";

const pub = (name: string, extra: Partial<PublishedItem> = {}): PublishedItem => ({ artifactId: "a1", path: `/workspace/outputs/${name}`, name, version: 1, mimeType: "", sizeBytes: 2048, ...extra });
const card = (p: PublishedItem) =>
  render(
    <ArtifactContext.Provider value="t1">
      <ArtifactCard pub={p} />
    </ArtifactContext.Provider>,
  );

describe("artifact helpers", () => {
  it("classifies files by extension or mime type", () => {
    expect(artifactKind("a.html")).toBe("html");
    expect(artifactKind("a.md")).toBe("markdown");
    expect(artifactKind("a.png")).toBe("image");
    expect(artifactKind("a", "application/pdf")).toBe("pdf");
    expect(artifactKind("a.csv")).toBe("csv");
    expect(artifactKind("a.json")).toBe("text");
    expect(artifactKind("a.bin")).toBe("other");
  });

  it("reads the artifact out of a publish_artifact result, and nothing out of an error message", () => {
    const ok = JSON.stringify({ published: { artifactId: "a1", path: "/workspace/r.html", name: "r.html", title: "R", version: 2, mimeType: "text/html", sizeBytes: 9 } });
    expect(publishedFromResult(ok)).toMatchObject({ artifactId: "a1", path: "/workspace/r.html", title: "R", version: 2 });
    expect(publishedFromResult("/workspace/x does not exist")).toBeNull();
    expect(publishedFromResult(undefined)).toBeNull();
  });

  it("parses quoted CSV fields", () => {
    expect(parseCsv('a,b\n"x, y","say ""hi"""\n1,2')).toEqual([["a", "b"], ["x, y", 'say "hi"'], ["1", "2"]]);
  });
});

describe("ArtifactCard", () => {
  it("previews an HTML artifact in a sandboxed frame served by the preview route, and can show the source", async () => {
    const f = mockFetch({ "GET /api/threads/t1/files/download?path=%2Fworkspace%2Foutputs%2Fr.html": "<h1>Report</h1>" });
    card(pub("r.html", { title: "The report" }));
    const frame = screen.getByTitle("The report") as HTMLIFrameElement;
    expect(frame.getAttribute("src")).toBe("/api/threads/t1/files/preview?path=%2Fworkspace%2Foutputs%2Fr.html");
    expect(frame.getAttribute("sandbox")).toBe("allow-scripts allow-popups"); // never allow-same-origin
    expect(screen.getByText("The report")).toBeTruthy();
    expect(f.calls).toHaveLength(0); // the frame loads by itself: nothing fetched for a preview
    fireEvent.click(screen.getByRole("tab", { name: "source" }));
    expect(await screen.findByText("<h1>Report</h1>")).toBeTruthy();
  });

  it("renders markdown and CSV from the file's text, an image through the preview route", async () => {
    mockFetch({
      "GET /api/threads/t1/files/download?path=%2Fworkspace%2Foutputs%2Fn.md": "# Heading\n\nbody",
      "GET /api/threads/t1/files/download?path=%2Fworkspace%2Foutputs%2Fd.csv": "name,qty\nbolt,3",
    });
    const a = card(pub("n.md"));
    expect(await screen.findByRole("heading", { name: "Heading" })).toBeTruthy();
    a.unmount();
    const b = card(pub("d.csv"));
    expect(await screen.findByRole("columnheader", { name: "qty" })).toBeTruthy();
    expect(screen.getByRole("cell", { name: "bolt" })).toBeTruthy();
    b.unmount();
    card(pub("p.png"));
    expect(screen.getByRole("img").getAttribute("src")).toBe("/api/threads/t1/files/preview?path=%2Fworkspace%2Foutputs%2Fp.png");
  });

  it("downloads through the files route", async () => {
    const f = mockFetch({ "GET /api/threads/t1/files/download?path=%2Fworkspace%2Foutputs%2Fr.html": new Response("x") });
    // jsdom has no object URLs.
    URL.createObjectURL = () => "blob:x";
    URL.revokeObjectURL = () => {};
    card(pub("r.html"));
    fireEvent.click(screen.getByRole("button", { name: /Download/ }));
    await waitFor(() => expect(f.find("GET", "/api/threads/t1/files/download")).toHaveLength(1));
  });

  it("a file type without a preview opens on its source", async () => {
    mockFetch({ "GET /api/threads/t1/files/download?path=%2Fworkspace%2Foutputs%2Fx.rs": "fn main() {}" });
    card(pub("x.rs"));
    expect(screen.queryByRole("tab")).toBeNull();
    expect(await screen.findByText("fn main() {}")).toBeTruthy();
  });
});

describe("publish_artifact in the transcript", () => {
  const msgs: Message[] = [
    { id: "u1", role: "user", content: "make it", createdAt: "2026-01-01T00:00:00Z" },
    { id: "a1", role: "assistant", content: "", createdAt: "2026-01-01T00:00:00Z", toolCalls: [{ id: "c1", type: "function", function: { name: "publish_artifact", arguments: '{"path":"r.html"}' } }, { id: "c2", type: "function", function: { name: "publish_artifact", arguments: '{"path":"nope"}' } }] },
    { id: "t1", role: "tool", toolCallId: "c1", content: JSON.stringify({ published: { artifactId: "a1", path: "/workspace/r.html", name: "r.html", version: 1, mimeType: "text/html", sizeBytes: 5 } }), createdAt: "2026-01-01T00:00:00Z" },
    { id: "t2", role: "tool", toolCallId: "c2", content: "/workspace/nope does not exist", error: "tool error", createdAt: "2026-01-01T00:00:00Z" },
  ];

  it("turns a published artifact into a card part and leaves a refused call a tool step", () => {
    const m = messagesFromThread(msgs).find((x) => x.role === "assistant");
    const content = toThreadMessage(m!).content as unknown as { type: string }[];
    expect(content.map((c) => c.type)).toEqual(["data-artifact", "tool-call"]);
  });
});

describe("PublishedList", () => {
  it("lists the thread's artifacts, newest first, and says nothing without any", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const list = (rows: unknown[]) => {
      mockFetch({ "GET /api/threads/t1/published": rows });
      return render(
        <QueryClientProvider client={qc}>
          <PublishedList threadId="t1" />
        </QueryClientProvider>,
      );
    };
    const a = list([]);
    await waitFor(() => expect(qc.isFetching()).toBe(0));
    expect(a.container.innerHTML).toBe("");
    a.unmount();
    qc.clear();
    list([
      { artifactId: "a1", path: "/workspace/one.html", name: "one.html", version: 1, mimeType: "text/html", sizeBytes: 10, publishedAt: "2026-01-01T00:00:00Z" },
      { artifactId: "a2", path: "/workspace/two.csv", name: "two.csv", title: "Two", version: 1, mimeType: "text/csv", sizeBytes: 10, publishedAt: "2026-01-01T00:00:00Z" },
    ]);
    expect(await screen.findByText("Artifacts (2)")).toBeTruthy();
    expect(screen.getAllByRole("button").map((b) => b.textContent)).toEqual([expect.stringContaining("Two"), expect.stringContaining("one.html")]);
  });
});
