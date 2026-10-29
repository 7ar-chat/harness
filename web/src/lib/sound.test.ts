import { describe, expect, it } from "vitest";
import { createScheduler, soundFor, strongest, type Sound } from "./sound";

const fin = (status: string, extra: Record<string, string> = {}) => ({ type: "run.finished" as const, status, threadId: "t1", actor: "bob", ...extra });

describe("soundFor", () => {
  it("maps run endings to sounds", () => {
    expect(soundFor(fin("completed"), { me: "alice" })).toBe("success");
    expect(soundFor(fin("waiting_approval"), { me: "alice" })).toBe("help");
    expect(soundFor(fin("waiting_input"), { me: "alice" })).toBe("help");
    expect(soundFor(fin("failed"), { me: "alice" })).toBe("failed");
    expect(soundFor(fin("cancelled"), { me: "alice" })).toBeNull();
  });
  it("ignores other event types", () => {
    expect(soundFor({ type: "run.started", threadId: "t1", actor: "bob" }, {})).toBeNull();
    expect(soundFor({ type: "message.sent", threadId: "t1", actor: "bob" }, {})).toBeNull();
  });
  it("is silent for the session you are looking at", () => {
    expect(soundFor(fin("completed"), { me: "alice", watching: "t1" })).toBeNull();
    expect(soundFor(fin("waiting_approval"), { me: "alice", watching: "t1" })).toBeNull();
    expect(soundFor(fin("completed"), { me: "alice", watching: "t2" })).toBe("success");
  });
  it("plays success for your own runs? no, but needs-help yes", () => {
    expect(soundFor(fin("completed", { actor: "alice" }), { me: "alice" })).toBeNull();
    expect(soundFor(fin("waiting_approval", { actor: "alice" }), { me: "alice" })).toBe("help");
  });
});

describe("scheduler", () => {
  it("plays one sound per burst and needs-help wins", () => {
    let t = 0;
    const timers: (() => void)[] = [];
    const played: Sound[] = [];
    const push = createScheduler((s) => played.push(s), { now: () => t, setTimer: (f) => timers.push(f) });
    push("success");
    push("help");
    push("success");
    timers.shift()?.();
    expect(played).toEqual(["help"]);
    t = 500;
    push("success"); // inside the quiet gap
    expect(timers.length).toBe(0);
    t = 2000;
    push("success");
    timers.shift()?.();
    expect(played).toEqual(["help", "success"]);
  });
  it("ranks sounds", () => {
    expect(strongest("success", "failed")).toBe("failed");
    expect(strongest("help", "failed")).toBe("help");
  });
});
