import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { mockFetch } from "@/test/fetch";
import { me, overview, project, shellRoutes } from "@/test/fixtures";
import { renderApp } from "@/test/render";

const page = () => within(screen.getByRole("main"));

const view = (over: object = {}, setting: object = {}) => ({
  setting: { enabled: false, weekday: 1, hour: 9, timezone: "UTC", ...setting },
  digests: [] as unknown[],
  canEdit: true,
  notified: false,
  ...over,
});

const routes = (role: "owner" | "member", digest: unknown, extra: Record<string, unknown> = {}) => {
  const p = project("p1", { name: "payments", role });
  return {
    ...shellRoutes(overview({ projects: [{ ...p, threads: [] }] }), me({ projects: [p] })),
    "GET /api/projects/p1": p,
    "GET /api/projects/p1/insights": { empty: true, summary: "", cost: { byDay: [], models: [] }, delivery: {}, flow: { weeks: [] }, attention: {}, people: [], tasks: [] },
    "GET /api/projects/p1/digest": digest,
    ...extra,
  };
};

describe("Weekly digest card", () => {
  it("lets the owner switch it on with a day, hour and zone", async () => {
    const saved = view({}, { enabled: true, weekday: 5, hour: 17, timezone: "Europe/Berlin", nextAt: "2026-11-27T16:00:00Z" });
    const m = mockFetch(routes("owner", view(), { "PUT /api/projects/p1/digest": saved }));
    await renderApp("/p/p1?tab=insights");
    const card = within(await screen.findByTestId("digest-card"));
    const save = card.getByRole("button", { name: "Save" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    fireEvent.click(card.getByRole("switch", { name: "Send the weekly digest" }));
    fireEvent.change(card.getByLabelText("Day"), { target: { value: "5" } });
    fireEvent.change(card.getByLabelText("Time"), { target: { value: "17" } });
    fireEvent.change(card.getByLabelText("Time zone"), { target: { value: "Europe/Berlin" } });
    expect(save.disabled).toBe(false);
    fireEvent.click(save);
    await waitFor(() => expect(m.find("PUT", "/api/projects/p1/digest")).toHaveLength(1));
    expect(m.find("PUT", "/api/projects/p1/digest")[0]?.body).toEqual({ enabled: true, weekday: 5, hour: 17, timezone: "Europe/Berlin" });
    expect(await card.findByText(/Next one:/)).toBeTruthy();
  });

  it("sends one now and lists the sent digests with links to their reports", async () => {
    const sent = { id: "dg_1", createdAt: "2026-11-24T09:00:00Z", from: "2026-11-17T09:00:00Z", to: "2026-11-24T09:00:00Z", summary: "Merged 3 PRs", trigger: "manual", url: "/api/projects/p1/digest/dg_1/report" };
    let n = 0;
    const m = mockFetch(
      routes("owner", () => view({ digests: n ? [sent] : [] }, { enabled: true }), {
        "POST /api/projects/p1/digest/send": () => {
          n++;
          return { digest: sent };
        },
      }),
    );
    await renderApp("/p/p1?tab=insights");
    const card = within(await screen.findByTestId("digest-card"));
    expect(card.getByText("None yet.")).toBeTruthy();
    fireEvent.click(card.getByRole("button", { name: /Send one now/ }));
    await waitFor(() => expect(m.find("POST", "/api/projects/p1/digest/send")).toHaveLength(1));
    const list = await card.findByRole("list", { name: "Sent digests" });
    expect(within(list).getByText("Merged 3 PRs")).toBeTruthy();
    expect(within(list).getByText("sent on request")).toBeTruthy();
    expect(within(list).getByRole("link", { name: "Open report" }).getAttribute("href")).toBe("/api/projects/p1/digest/dg_1/report");
    expect(within(list).getByRole("link", { name: /Download the report/ }).getAttribute("href")).toBe("/api/projects/p1/digest/dg_1/report?download=1");
  });

  it("is read-only for a member", async () => {
    mockFetch(routes("member", view({ canEdit: false }, { enabled: true, nextAt: "2026-11-30T09:00:00Z" })));
    await renderApp("/p/p1?tab=insights");
    const card = within(await screen.findByTestId("digest-card"));
    expect(card.queryByRole("button", { name: /Send one now/ })).toBeNull();
    expect(card.queryByRole("switch")).toBeNull();
    expect(card.getByText(/Sent weekly, next/)).toBeTruthy();
  });

  it("is not shown on a server without digests", async () => {
    mockFetch(routes("owner", undefined));
    await renderApp("/p/p1?tab=insights");
    await page().findByTestId("insights");
    expect(screen.queryByTestId("digest-card")).toBeNull();
  });
});
