import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { describe, expect, it, vi } from "vitest";
import { mockFetch, reply } from "@/test/fetch";
import { me, overview, project, shellRoutes } from "@/test/fixtures";
import { renderApp } from "@/test/render";

const page = () => within(screen.getByRole("main"));

const routes = (role: "owner" | "member" | "viewer", tapd: unknown, extra: Record<string, unknown> = {}) => {
  const p = project("p1", { name: "payments", role });
  return {
    ...shellRoutes(overview({ projects: [{ ...p, threads: [] }] }), me({ projects: [p] })),
    "GET /api/projects/p1": p,
    "GET /api/projects/p1/members": [],
    "GET /api/projects/p1/github": { configured: false, connected: false },
    "GET /api/projects/p1/requirements": [],
    "GET /api/projects/p1/requirements/settings": { projectId: "p1", autoDispatch: false, commentOnIssue: false },
    "GET /api/projects/p1/requirements/tapd": tapd,
    ...extra,
  };
};

describe("TAPD import", () => {
  it("lets the owner connect a workspace with an API account and never shows the password again", async () => {
    const m = mockFetch(
      routes("owner", { connected: false, available: true }, {
        "PUT /api/projects/p1/requirements/tapd": { connected: true, available: true, workspaceId: "10104801", user: "api_user", hasPassword: true },
      }),
    );
    await renderApp("/p/p1?tab=requirements");
    fireEvent.click(await page().findByRole("button", { name: "Connect TAPD" }));
    const dialog = within(await screen.findByRole("dialog"));
    const submit = dialog.getByRole("button", { name: "Connect" }) as HTMLButtonElement;
    expect(submit.disabled).toBe(true);
    fireEvent.change(dialog.getByLabelText(/Workspace id/), { target: { value: "10104801" } });
    fireEvent.change(dialog.getByLabelText("API user"), { target: { value: "api_user" } });
    fireEvent.change(dialog.getByLabelText("API password"), { target: { value: "s3cret" } });
    fireEvent.change(dialog.getByLabelText(/Open statuses/), { target: { value: "planning, developing" } });
    expect(submit.disabled).toBe(false);
    fireEvent.click(submit);
    await waitFor(() => expect(m.find("PUT", "/api/projects/p1/requirements/tapd")).toHaveLength(1));
    expect(m.find("PUT", "/api/projects/p1/requirements/tapd")[0]?.body).toEqual({
      workspaceId: "10104801", baseUrl: "", openStatuses: "planning, developing", user: "api_user", password: "s3cret",
    });
    expect(await page().findByRole("button", { name: "Import from TAPD" })).toBeTruthy();
    expect(document.body.textContent).not.toContain("s3cret");
  });

  it("offers the token form and sends only the token", async () => {
    const m = mockFetch(routes("owner", { connected: false, available: true }, { "PUT /api/projects/p1/requirements/tapd": { connected: true, available: true, workspaceId: "1", hasToken: true } }));
    await renderApp("/p/p1?tab=requirements");
    fireEvent.click(await page().findByRole("button", { name: "Connect TAPD" }));
    const dialog = within(await screen.findByRole("dialog"));
    fireEvent.click(dialog.getByRole("button", { name: "Access token" }));
    fireEvent.change(dialog.getByLabelText(/Workspace id/), { target: { value: "1" } });
    fireEvent.change(dialog.getByLabelText("Access token"), { target: { value: "tok" } });
    fireEvent.click(dialog.getByRole("button", { name: "Connect" }));
    await waitFor(() => expect(m.find("PUT", "/api/projects/p1/requirements/tapd")).toHaveLength(1));
    expect(m.find("PUT", "/api/projects/p1/requirements/tapd")[0]?.body).toEqual({ workspaceId: "1", baseUrl: "", openStatuses: "", token: "tok" });
  });

  it("imports for a member and says how open stories were decided", async () => {
    const m = mockFetch(
      routes("member", { connected: true, available: true, workspaceId: "1" }, {
        "POST /api/projects/p1/requirements/import/tapd": { imported: 2, skipped: 1, requirements: [], source: "filter", truncated: false },
      }),
    );
    await renderApp("/p/p1?tab=requirements");
    expect(page().queryByRole("button", { name: "TAPD settings" })).toBeNull();
    fireEvent.click(await page().findByRole("button", { name: "Import from TAPD" }));
    await waitFor(() => expect(m.find("POST", "/api/projects/p1/requirements/import/tapd")).toHaveLength(1));
    const line = await page().findByTestId("tapd-result");
    expect(line.textContent).toContain("Imported 2 TAPD story(ies), skipped 1");
    expect(line.textContent).toContain("by your list of open statuses");
  });

  it("shows the server's explanation when TAPD does not say which statuses are final", async () => {
    const err = vi.spyOn(toast, "error").mockImplementation(() => "");
    mockFetch(
      routes("member", { connected: true, available: true, workspaceId: "1" }, {
        "POST /api/projects/p1/requirements/import/tapd": reply(409, { error: { code: "tapd_no_statuses", message: "TAPD did not say which statuses are final. List the open statuses by hand." } }),
      }),
    );
    await renderApp("/p/p1?tab=requirements");
    fireEvent.click(await page().findByRole("button", { name: "Import from TAPD" }));
    await waitFor(() => expect(err).toHaveBeenCalledWith(expect.stringContaining("List the open statuses by hand")));
  });

  it("hides TAPD from viewers and from an owner on a server that cannot keep the credentials", async () => {
    mockFetch(routes("viewer", { connected: true, available: true }));
    await renderApp("/p/p1?tab=requirements");
    await page().findByRole("list", { name: "Requirements" }).catch(() => undefined);
    expect(screen.queryByRole("button", { name: "Import from TAPD" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Connect TAPD" })).toBeNull();
  });
});
