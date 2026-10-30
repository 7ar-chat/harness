import { beforeEach, describe, expect, it, vi } from "vitest";
import { isNewerVisit, isUnread, loadLocalSeen, saveLocalSeen } from "./unread";

describe("isUnread", () => {
  it("never flags a thread that has not been seen yet, or the one that is open", () => {
    expect(isUnread({}, "t1", "2026-01-02T00:00:00Z")).toBe(false);
    expect(isUnread({ t1: "2026-01-01T00:00:00Z" }, "t1", "2026-01-02T00:00:00Z", "t1")).toBe(false);
  });
  it("flags activity after the last visit only", () => {
    expect(isUnread({ t1: "2026-01-01T00:00:00Z" }, "t1", "2026-01-02T00:00:00Z")).toBe(true);
    expect(isUnread({ t1: "2026-01-02T00:00:00Z" }, "t1", "2026-01-02T00:00:00Z")).toBe(false);
    expect(isUnread({ t1: "2026-01-03T00:00:00Z" }, "t1", "2026-01-02T00:00:00Z")).toBe(false);
  });
  it("compares instants, not text: server timestamps carry fractional seconds and offsets", () => {
    expect(isUnread({ t1: "2026-01-02T00:00:00.500Z" }, "t1", "2026-01-02T00:00:00Z")).toBe(false);
    expect(isUnread({ t1: "2026-01-02T01:00:00+01:00" }, "t1", "2026-01-02T00:00:01Z")).toBe(true);
  });
});

describe("isNewerVisit", () => {
  it("accepts a first visit and a later one, never an equal or older one", () => {
    expect(isNewerVisit({}, "t1", "2026-01-02T00:00:00Z")).toBe(true);
    expect(isNewerVisit({ t1: "2026-01-02T00:00:00Z" }, "t1", "2026-01-03T00:00:00Z")).toBe(true);
    expect(isNewerVisit({ t1: "2026-01-02T00:00:00Z" }, "t1", "2026-01-02T00:00:00Z")).toBe(false);
    expect(isNewerVisit({ t1: "2026-01-02T00:00:00Z" }, "t1", "2026-01-01T00:00:00Z")).toBe(false);
  });
});

describe("local fallback storage", () => {
  beforeEach(() => localStorage.clear());

  it("round-trips the map", () => {
    // Act
    saveLocalSeen({ a: "2026-01-05T00:00:00Z" });
    // Assert
    expect(loadLocalSeen()).toEqual({ a: "2026-01-05T00:00:00Z" });
  });

  it("survives corrupt or unexpected stored data and unavailable storage", () => {
    localStorage.setItem("harness.seen", "{not json");
    expect(loadLocalSeen()).toEqual({});
    localStorage.setItem("harness.seen", "[1]");
    expect(loadLocalSeen()).toEqual({});
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("denied");
    });
    expect(() => saveLocalSeen({ a: "x" })).not.toThrow();
    vi.restoreAllMocks();
  });
});
