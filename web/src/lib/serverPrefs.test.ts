import { afterEach, describe, expect, it, vi } from "vitest";
import { patchOnboarding, settleOnboarding, type OnboardingState } from "./onboarding";
import { applyUi, debouncedPusher, fetchPrefs, mergeKey, pushPrefs, rememberUi } from "./serverPrefs";

afterEach(() => vi.useRealTimers());

describe("mergeKey: the later change wins, an unstamped browser value never overwrites", () => {
  it("takes the server's value when it is newer, keeps and pushes the browser's when it is newer", () => {
    expect(mergeKey({ dismissed: true, at: 5 }, { welcomed: true, at: 9 })).toMatchObject({ value: { welcomed: true, at: 9 }, push: false, adopted: true });
    expect(mergeKey({ dismissed: true, at: 9 }, { welcomed: true, at: 5 })).toMatchObject({ value: { dismissed: true, at: 9 }, push: true, adopted: false });
  });
  it("adopts a value only one side has, and pushes only what the person changed here", () => {
    expect(mergeKey(undefined, { toured: true, at: 3 })).toMatchObject({ push: false, adopted: true });
    expect(mergeKey({ toured: true, at: 3 }, undefined)).toMatchObject({ push: true });
    expect(mergeKey({ toured: true }, undefined)).toMatchObject({ push: false }); // legacy or seeded: no stamp
    expect(mergeKey({ toured: true }, { dismissed: true, at: 1 })).toMatchObject({ value: { dismissed: true, at: 1 }, adopted: true });
  });
});

describe("settleOnboarding: the browser and the server agree after one call", () => {
  it("shows what was done in another browser", () => {
    const push = vi.fn();
    settleOnboarding("ann", { welcomed: true, dismissed: true, at: 100 }, push);
    expect(JSON.parse(localStorage.getItem("7ar.onboarding.v1.ann")!)).toEqual({ welcomed: true, dismissed: true, at: 100 });
    expect(push).not.toHaveBeenCalled();
  });
  it("keeps newer local progress and sends it to the server", () => {
    localStorage.setItem("7ar.onboarding.v1.ann", JSON.stringify({ welcomed: true, at: 500 }));
    const push = vi.fn();
    settleOnboarding("ann", { welcomed: true, dismissed: true, at: 100 }, push);
    expect(push).toHaveBeenCalledWith({ welcomed: true, at: 500 });
    expect(JSON.parse(localStorage.getItem("7ar.onboarding.v1.ann")!).dismissed).toBeUndefined();
  });
  it("does nothing for a person with nothing anywhere", () => {
    const push = vi.fn();
    settleOnboarding("ann", undefined, push);
    expect(push).not.toHaveBeenCalled();
    expect(localStorage.getItem("7ar.onboarding.v1.ann")).toBeNull();
  });
  it("stamps a local change so it can win later", () => {
    patchOnboarding("ann", { welcomed: true });
    const s = JSON.parse(localStorage.getItem("7ar.onboarding.v1.ann")!) as OnboardingState;
    expect(s.welcomed).toBe(true);
    expect(s.at).toBeGreaterThan(0);
  });
});

describe("the server is optional", () => {
  it("falls back silently when the server cannot be reached or answers badly", async () => {
    expect(await fetchPrefs(() => Promise.reject(new Error("offline")))).toBeNull();
    expect(await fetchPrefs(() => Promise.resolve({} as never))).toBeNull();
    expect(await fetchPrefs(() => Promise.resolve({ values: { a: 1 } }))).toEqual({ values: { a: 1 } });
    expect(await pushPrefs({ a: 1 }, () => Promise.reject(new Error("offline")))).toBe(false);
    const sent = vi.fn(() => Promise.resolve());
    expect(await pushPrefs({ a: 1 }, sent)).toBe(true);
    expect(sent).toHaveBeenCalledWith("/me/preferences", { values: { a: 1 } });
  });
  it("sends a burst of changes as one request with the last value of each key", () => {
    vi.useFakeTimers();
    const push = vi.fn(() => Promise.resolve(true));
    const p = debouncedPusher(400, push);
    p.add("ui", { at: 1 });
    p.add("ui", { at: 2 });
    p.add("onboarding", { at: 3 });
    vi.advanceTimersByTime(399);
    expect(push).not.toHaveBeenCalled();
    vi.advanceTimersByTime(2);
    expect(push).toHaveBeenCalledTimes(1);
    expect(push).toHaveBeenCalledWith({ ui: { at: 2 }, onboarding: { at: 3 } });
  });
});

describe("the layout choices follow the person", () => {
  it("writes the server's folded sections and sizes into this browser when they are newer", () => {
    applyUi({ at: 50, sections: { Setup: false }, explorerW: 320, panelH: 200 });
    expect(localStorage.getItem("7ar.ui.section.Setup")).toBe("0");
    expect(localStorage.getItem("7ar.ui.explorerW")).toBe("320");
    expect(localStorage.getItem("7ar.ui.panelH")).toBe("200");
  });
  it("keeps a newer local choice and ignores an unchanged one", () => {
    vi.useFakeTimers();
    vi.stubGlobal("fetch", vi.fn(async () => new Response("{}", { status: 200 })));
    rememberUi({ explorerW: 300 });
    const stamp = (JSON.parse(localStorage.getItem("7ar.ui.server.v1")!) as { at: number }).at;
    vi.advanceTimersByTime(10);
    rememberUi({ explorerW: 300 }); // same value: no new stamp
    expect((JSON.parse(localStorage.getItem("7ar.ui.server.v1")!) as { at: number }).at).toBe(stamp);
    applyUi({ at: stamp - 1000, explorerW: 400 });
    expect(localStorage.getItem("7ar.ui.explorerW")).toBeNull();
  });
});
