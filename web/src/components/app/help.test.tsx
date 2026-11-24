import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { mockFetch, reply } from "@/test/fetch";
import { me, overview, project, shellRoutes } from "@/test/fixtures";
import { renderApp } from "@/test/render";

const p = project("p1", { name: "payments" });
const routes = () => ({
  ...shellRoutes(overview({ projects: [{ ...p, threads: [] }] }), me({ projects: [p] })),
  "GET /api/projects/p1": p,
  "GET /api/memory/stats": reply(404),
  "GET /api/projects/p1/tasks?limit=500": [],
});

describe("help inside a form dialog", () => {
  it("the ? beside Policy opens the article over the dialog; Escape closes only the article", async () => {
    mockFetch(routes());
    await renderApp("/p/p1?tab=tasks");
    fireEvent.click(await within(screen.getByRole("main")).findByRole("button", { name: "Create your first task" }));
    const form = await screen.findByRole("dialog", { name: "New task" });
    fireEvent.click(within(form).getByRole("button", { name: "Policy (help)" }));
    const article = await screen.findByRole("dialog", { name: /Policies/ });
    fireEvent.keyDown(article, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("dialog", { name: /Policies/ })).toBeNull());
    expect(screen.getByRole("dialog", { name: "New task" })).toBeTruthy();
    expect(within(screen.getByRole("dialog", { name: "New task" })).getByRole("button", { name: /Advanced options/ })).toBeTruthy();
  });
});
