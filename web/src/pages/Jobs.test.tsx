import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { mockFetch, reply } from "@/test/fetch";
import { shellRoutes } from "@/test/fixtures";
import { renderApp } from "@/test/render";

const profile = { enabled: false, mode: "queue", titles: ["go"], minScore: 4.5, queueScore: 4, dailyCap: 10, companies: [{ name: "Acme", ats: "greenhouse", slug: "acme" }] };
const job = (over: object) => ({ id: "j1", company: "Acme", title: "Go Engineer", location: "Paris", url: "https://x.test/1", status: "queued", score: 4.3, reason: "Fits the Go work", seen: "2026-11-05T10:00:00Z", ...over });
const page = () => within(screen.getByRole("main"));
const overview = (jobs: object[], counts: Record<string, number> = { queued: 1 }) => ({ profile, counts, jobs });

describe("JobsPage", () => {
  it("shows what waits for you and approves a job on Apply", async () => {
    const m = mockFetch({ ...shellRoutes(), "GET /api/me/jobs": overview([job({})]), "PATCH /api/me/jobs/j1": job({ status: "approved" }) });
    await renderApp("/jobs");
    expect(await page().findByText("Go Engineer")).toBeTruthy();
    expect(page().getByText("Fits the Go work")).toBeTruthy();
    fireEvent.click(page().getByRole("button", { name: /Apply/ }));
    await waitFor(() => expect(m.find("PATCH", "/api/me/jobs/j1")).toHaveLength(1));
    expect(m.find("PATCH", "/api/me/jobs/j1")[0]?.body).toEqual({ status: "approved" });
  });

  it("saves the profile with the switches and lists as typed", async () => {
    const m = mockFetch({ ...shellRoutes(), "GET /api/me/jobs": overview([], {}), "PUT /api/me/jobs/profile": profile });
    await renderApp("/jobs");
    fireEvent.change(await page().findByLabelText("Roles"), { target: { value: "go, rust" } });
    fireEvent.click(page().getByRole("switch", { name: "Search around the clock" }));
    fireEvent.click(page().getByRole("button", { name: "Save" }));
    await waitFor(() => expect(m.find("PUT", "/api/me/jobs/profile")).toHaveLength(1));
    const body = m.find("PUT", "/api/me/jobs/profile")[0]?.body as { titles: string[]; enabled: boolean; mode: string };
    expect(body.titles).toEqual(["go", "rust"]);
    expect(body.enabled).toBe(true);
    expect(body.mode).toBe("queue");
  });

  it("opens the job agent's own conversation", async () => {
    const m = mockFetch({ ...shellRoutes(), "GET /api/me/jobs": overview([], {}), "POST /api/me/jobs/thread": { threadId: "jt1" }, "GET /api/threads/jt1": reply(404) });
    await renderApp("/jobs");
    fireEvent.click(await page().findByRole("button", { name: /Talk to the job agent/ }));
    await waitFor(() => expect(m.find("POST", "/api/me/jobs/thread")).toHaveLength(1));
    await waitFor(() => expect(window.location.hash).toContain("/t/jt1"));
  });

  it("says so when the server has no job search", async () => {
    mockFetch({ ...shellRoutes(), "GET /api/me/jobs": reply(404, { error: { code: "not_found", message: "no" } }) });
    await renderApp("/jobs");
    expect(await page().findByText("This server has no job search.")).toBeTruthy();
  });
});
