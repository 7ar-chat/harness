import { fireEvent, screen, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { mockFetch } from "@/test/fetch";
import { me, overview, project, shellRoutes, thread } from "@/test/fixtures";
import { renderApp } from "@/test/render";

const templates = [
  { id: "react-vite", name: "React + Vite", description: "React 19 single-page app with Vite and hot reload." },
  { id: "astro", name: "Astro", description: "Content-focused website." },
];

describe("New project", () => {
  const open = async (extra: Record<string, unknown> = {}) => {
    const m = mockFetch({
      ...shellRoutes(overview({}), me({})),
      "GET /api/templates": templates,
      "POST /api/projects": project("p9", { name: "Site" }),
      "GET /api/projects/p9": project("p9", { name: "Site" }),
      ...extra,
    });
    await renderApp("/");
    fireEvent.click((await screen.findAllByRole("button", { name: "New project" }))[0]!);
    await screen.findByRole("radiogroup", { name: "Template" });
    return m;
  };

  it("offers the templates and a blank project, and creates from the chosen template without a repository", async () => {
    const m = await open();
    expect(await screen.findByRole("radio", { name: /Blank/ })).toBeTruthy();
    fireEvent.click(await screen.findByRole("radio", { name: /Astro/ }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Site" } });
    fireEvent.click(screen.getByRole("button", { name: "Create project" }));
    await waitFor(() => expect(m.find("POST", "/api/projects")).toHaveLength(1));
    expect(m.find("POST", "/api/projects")[0]?.body).toEqual({ name: "Site", description: "", template: "astro" });
  });

  it("sends a blank project with no template", async () => {
    const m = await open();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Site" } });
    fireEvent.click(screen.getByRole("button", { name: "Create project" }));
    await waitFor(() => expect(m.find("POST", "/api/projects")).toHaveLength(1));
    expect(m.find("POST", "/api/projects")[0]?.body).toEqual({ name: "Site", description: "" });
  });

  it("switches to an existing repository and sends its URL and branch instead of a template", async () => {
    const m = await open();
    fireEvent.click(screen.getByRole("tab", { name: "Existing repository" }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Site" } });
    fireEvent.change(screen.getByLabelText("Repository URL"), { target: { value: "https://github.com/acme/site" } });
    fireEvent.click(screen.getByRole("button", { name: "Create project" }));
    await waitFor(() => expect(m.find("POST", "/api/projects")).toHaveLength(1));
    expect(m.find("POST", "/api/projects")[0]?.body).toEqual({ name: "Site", description: "", repoUrl: "https://github.com/acme/site", defaultBranch: "main" });
  });
});

describe("New session", () => {
  it("creates a session directly without prompting for a title", async () => {
    const m = mockFetch({
      ...shellRoutes(overview({}), me({})),
      "POST /api/threads": thread("t-new", { title: "" }),
      "GET /api/threads/t-new": { id: "t-new", title: "", kind: "chat", status: "idle", createdBy: "alice", createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-01-01T00:00:00Z", messages: [], activeRun: null, pendingApproval: null, pendingQuestion: null },
      "GET /api/threads/t-new/files": [],
    });
    await renderApp("/");
    fireEvent.click(await screen.findByRole("button", { name: "New personal session" }));
    await waitFor(() => expect(m.find("POST", "/api/threads")).toHaveLength(1));
    expect(m.find("POST", "/api/threads")[0]?.body).toEqual({});
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});
