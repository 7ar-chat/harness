import { act, renderHook } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { useRunSettings, useStored } from "./hooks";

describe("useStored", () => {
  it("starts from the stored JSON and writes changes back", () => {
    localStorage.setItem("k", JSON.stringify({ a: 1 }));
    const { result } = renderHook(() => useStored("k", { a: 0 }));
    expect(result.current[0]).toEqual({ a: 1 });
    act(() => result.current[1]({ a: 2 }));
    expect(result.current[0]).toEqual({ a: 2 });
    expect(localStorage.getItem("k")).toBe('{"a":2}');
  });
  it("falls back to the initial value for missing or corrupt storage", () => {
    expect(renderHook(() => useStored("missing", 7)).result.current[0]).toBe(7);
    localStorage.setItem("bad", "{oops");
    expect(renderHook(() => useStored("bad", "dflt")).result.current[0]).toBe("dflt");
  });
  it("keeps the in-memory value when storage rejects writes", () => {
    const { result } = renderHook(() => useStored("k", 1));
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("denied");
    });
    act(() => result.current[1](2));
    expect(result.current[0]).toBe(2);
  });
});

describe("useRunSettings", () => {
  it("defaults to interactive build mode and fills the model from the server default", () => {
    const { result } = renderHook(() => useRunSettings("gpt-x"));
    expect(result.current[0]).toEqual({ interactive: true, mode: "build", model: "gpt-x" });
  });
  it("a chosen model wins over the default, and a patch keeps the other settings", () => {
    const { result } = renderHook(() => useRunSettings("gpt-x"));
    act(() => result.current[1]({ model: "mine" }));
    act(() => result.current[1]({ mode: "plan" }));
    expect(result.current[0]).toEqual({ interactive: true, mode: "plan", model: "mine" });
    expect(JSON.parse(localStorage.getItem("harness.run") ?? "{}")).toEqual({ interactive: true, mode: "plan", model: "mine" });
  });
  it("does not persist the server default as if the user had chosen it", () => {
    const { result } = renderHook(() => useRunSettings("gpt-x"));
    act(() => result.current[1]({ interactive: false }));
    expect(JSON.parse(localStorage.getItem("harness.run") ?? "{}")).not.toHaveProperty("model");
  });
});
