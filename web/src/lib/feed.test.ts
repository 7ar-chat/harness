import { describe, expect, it } from "vitest";
import type { FeedEvent, Overview, ThreadSummary } from "@/api/types";
import { patchOverview } from "./feed";

const th = (id: string, extra: Partial<ThreadSummary> = {}): ThreadSummary => ({ id, title: `T ${id}`, status: "idle", createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-01-01T00:00:00Z", ...extra });
const overview = (): Overview => ({
  personal: [th("p1")],
  projects: [{ id: "pr", name: "proj", role: "owner", createdBy: "a", createdAt: "", updatedAt: "", threads: [th("t1"), th("t2")] }],
});
const ev = (e: Partial<FeedEvent>): FeedEvent => ({ type: "run.started", threadId: "t1", at: "2026-02-01T00:00:00Z", ...e });

describe("patchOverview", () => {
  it("marks the thread running and bumps its time, in a project and in the personal list", () => {
    const o = patchOverview(overview(), ev({}));
    expect(o.projects[0]?.threads[0]).toMatchObject({ id: "t1", status: "running", updatedAt: "2026-02-01T00:00:00Z" });
    expect(o.projects[0]?.threads[1]).toMatchObject({ id: "t2", status: "idle", updatedAt: "2026-01-01T00:00:00Z" });
    expect(patchOverview(overview(), ev({ threadId: "p1" })).personal[0]?.status).toBe("running");
  });

  it.each([
    ["completed", "idle"],
    ["cancelled", "idle"],
    ["failed", "failed"],
    ["interrupted", "failed"],
    ["waiting_approval", "waiting_approval"],
    ["waiting_input", "waiting_input"],
  ])("maps a run that finished %s to %s", (finished, status) => {
    const o = overview();
    o.projects[0]!.threads[0]!.status = "running";
    expect(patchOverview(o, ev({ type: "run.finished", status: finished })).projects[0]?.threads[0]?.status).toBe(status);
  });

  it("keeps the status for an unknown finish status or a non-run event, but still takes the title", () => {
    const o = overview();
    o.projects[0]!.threads[0]!.status = "running";
    expect(patchOverview(o, ev({ type: "run.finished", status: "weird" })).projects[0]?.threads[0]?.status).toBe("running");
    const renamed = patchOverview(o, ev({ type: "thread.updated", title: "New name" })).projects[0]?.threads[0];
    expect(renamed).toMatchObject({ status: "running", title: "New name" });
    expect(patchOverview(o, ev({ type: "message.sent" })).projects[0]?.threads[0]?.title).toBe("T t1");
  });

  it("removes a deleted thread everywhere", () => {
    const o = patchOverview(overview(), ev({ type: "thread.deleted" }));
    expect(o.projects[0]?.threads.map((t) => t.id)).toEqual(["t2"]);
    expect(patchOverview(overview(), ev({ type: "thread.deleted", threadId: "p1" })).personal).toEqual([]);
  });

  it("leaves threads it does not know about alone and does not mutate its input", () => {
    const before = overview();
    const snapshot = JSON.stringify(before);
    const o = patchOverview(before, ev({ threadId: "zzz" }));
    expect(o).toEqual(before);
    patchOverview(before, ev({}));
    expect(JSON.stringify(before)).toBe(snapshot);
  });
});
