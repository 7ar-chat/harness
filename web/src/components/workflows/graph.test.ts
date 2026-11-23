import { describe, expect, it } from "vitest";
import type { WorkflowNode } from "@/api/types";
import {
  ancestors, autoLayout, connect, cycleOf, disconnect, edgePath, edgesOf, freeId, inputsText, localIssues, moveNode, newNode, nextPos, parseInputs, parseList, removeNode, renameNode, reworkEdgesOf, wouldCycle,
} from "./graph";

const n = (id: string, deps: string[] = [], extra: Partial<WorkflowNode> = {}): WorkflowNode => ({ id, kind: "agent", prompt: "do", dependsOn: deps, pos: { x: 0, y: 0 }, ...extra });

describe("adding steps", () => {
  it("gives each kind the fields it needs and a free id", () => {
    const nodes = [n("agent")];
    const a = newNode("agent", nodes, { x: 43, y: 17 });
    const g = newNode("gate", nodes, { x: 0, y: 0 });
    const s = newNode("script", nodes, { x: 0, y: 0 });
    expect(a.id).toBe("agent2");
    expect(a.pos).toEqual({ x: 40, y: 20 }); // snapped to the grid
    expect(a.role).toBe("coder");
    expect(g.approvers).toEqual(["role:member"]);
    expect(s.command).toBe("");
    expect(newNode("subworkflow", [], { x: 0, y: 0 }).id).toBe("workflow");
    expect(freeId([n("a"), n("a2")], "a")).toBe("a3");
  });

  it("puts the next step to the right of the last", () => {
    expect(nextPos([])).toEqual({ x: 40, y: 40 });
    expect(nextPos([n("a", [], { pos: { x: 40, y: 40 } }), n("b", [], { pos: { x: 300, y: 100 } })]).y).toBe(100);
    expect(nextPos([n("a", [], { pos: { x: 40, y: 40 } })]).x).toBeGreaterThan(40 + 184);
  });
});

describe("connecting steps", () => {
  const graph = [n("a"), n("b", ["a"]), n("c", ["b"])];

  it("makes the target wait for the source", () => {
    const r = connect([n("a"), n("b")], "a", "b");
    expect(r.error).toBeUndefined();
    expect(r.nodes.find((x) => x.id === "b")?.dependsOn).toEqual(["a"]);
  });

  it("refuses itself, duplicates, unknown steps and loops", () => {
    expect(connect(graph, "a", "a").error).toMatch(/itself/);
    expect(connect(graph, "a", "b").error).toMatch(/already/);
    expect(connect(graph, "a", "zzz").error).toMatch(/Unknown/);
    expect(connect(graph, "c", "a").error).toMatch(/wait for each other/);
    expect(wouldCycle(graph, "c", "a")).toBe(true);
    expect(wouldCycle(graph, "a", "c")).toBe(false);
  });

  it("disconnects, and lists edges and ancestors", () => {
    expect(disconnect(graph, "a", "b").find((x) => x.id === "b")?.dependsOn).toEqual([]);
    expect(edgesOf(graph)).toEqual([{ from: "a", to: "b" }, { from: "b", to: "c" }]);
    expect(ancestors(graph, "c").sort()).toEqual(["a", "b"]);
  });

  it("finds a cycle that came from outside the editor", () => {
    expect(cycleOf([n("a", ["b"]), n("b", ["a"])])).toMatch(/a -> b -> a|b -> a -> b/);
    expect(cycleOf(graph)).toBe("");
  });
});

describe("removing and renaming steps", () => {
  it("removes a step with every reference to it", () => {
    const nodes = [n("a"), n("b", ["a"], { onFailure: { action: "rework", target: "a" } })];
    const out = removeNode(nodes, "a");
    expect(out).toHaveLength(1);
    expect(out[0]?.dependsOn).toEqual([]);
    expect(out[0]?.onFailure?.target).toBe("");
    expect(out[0]?.onFailure?.action).toBe("fail");
  });

  it("renames a step, its edges, rework targets and the references in prompts", () => {
    const nodes = [n("a"), n("b", ["a"], { prompt: "use {{nodes.a.result}} and {{nodes.a.outputs.plan}}", command: "echo {{nodes.a.result}}", onFailure: { action: "rework", target: "a" } })];
    const r = renameNode(nodes, "a", "plan");
    expect(r.error).toBeUndefined();
    expect(r.nodes[0]?.id).toBe("plan");
    const b = r.nodes[1]!;
    expect(b.dependsOn).toEqual(["plan"]);
    expect(b.prompt).toBe("use {{nodes.plan.result}} and {{nodes.plan.outputs.plan}}");
    expect(b.command).toBe("echo {{nodes.plan.result}}");
    expect(b.onFailure?.target).toBe("plan");
  });

  it("refuses a bad or taken id", () => {
    expect(renameNode([n("a"), n("b")], "a", "b").error).toMatch(/Another step/);
    expect(renameNode([n("a")], "a", "Bad Id").error).toMatch(/lowercase/);
    expect(renameNode([n("a")], "a", "a").error).toBeUndefined();
  });
});

describe("the checks while typing", () => {
  it("mirrors the server's rules", () => {
    const issues = localIssues("", [
      n("a", ["ghost"], { prompt: "" }),
      { id: "g", kind: "gate", dependsOn: [], approvers: [], pos: { x: 0, y: 0 } },
      { id: "s", kind: "script", command: " ", pos: { x: 0, y: 0 } },
      { id: "w", kind: "subworkflow", pos: { x: 0, y: 0 } },
      n("r", ["a"], { onFailure: { action: "rework" } }),
      n("t", [], { prompt: "{{inputs.nope}} {{nodes.zzz.result}}" }),
    ]);
    const text = issues.map((i) => `${i.nodeId ?? ""}:${i.message}`).join("\n");
    for (const want of ["needs a name", "ghost", "needs a prompt", "at least one approver", "nothing to review", "needs a command", "Choose the workflow", "needs the step to go back to", "no such input", "no such step"]) {
      expect(text, want).toContain(want);
    }
  });

  it("accepts a good graph and reports a cycle", () => {
    expect(localIssues("w", [n("a"), n("b", ["a"], { prompt: "{{inputs.task}} {{nodes.a.result}}" })], ["task"])).toEqual([]);
    expect(localIssues("w", [n("a", ["b"]), n("b", ["a"])]).some((i) => /cycle/.test(i.message))).toBe(true);
    expect(localIssues("w", []).some((i) => /first step/.test(i.message))).toBe(true);
  });
});

describe("layout and drawing", () => {
  it("lays steps out in columns by depth", () => {
    const out = autoLayout([n("a"), n("b", ["a"]), n("c", ["a"]), n("d", ["b", "c"])]);
    const x = (id: string) => out.find((o) => o.id === id)!.pos.x;
    const y = (id: string) => out.find((o) => o.id === id)!.pos.y;
    expect(x("a")).toBeLessThan(x("b"));
    expect(x("b")).toBe(x("c"));
    expect(y("b")).not.toBe(y("c"));
    expect(x("d")).toBeGreaterThan(x("b"));
  });

  it("does not hang on a cycle", () => {
    expect(autoLayout([n("a", ["b"]), n("b", ["a"])])).toHaveLength(2);
  });

  it("draws an edge from the right of one box to the left of the next", () => {
    const a = n("a", [], { pos: { x: 0, y: 0 } });
    const b = n("b", ["a"], { pos: { x: 300, y: 100 } });
    expect(edgePath(a, b)).toMatch(/^M 184 32 C .* 300 132$/);
    expect(reworkEdgesOf([a, n("t", ["a"], { onFailure: { action: "rework", target: "a" } })])).toEqual([{ from: "t", to: "a" }]);
    expect(reworkEdgesOf([a, { id: "g", kind: "gate", dependsOn: ["a"], approvers: ["x"], pos: { x: 0, y: 0 }, onFailure: { action: "rework" } }])).toEqual([{ from: "g", to: "a" }]);
    expect(moveNode([a], "a", 33, 47)[0]?.pos).toEqual({ x: 30, y: 50 });
  });
});

describe("text helpers", () => {
  it("parses lists and key=value lines", () => {
    expect(parseList("a, b\n\n c,a")).toEqual(["a", "b", "c"]);
    expect(parseInputs("task={{nodes.a.result}}\nbad\n k = v=w ")).toEqual({ task: "{{nodes.a.result}}", k: "v=w" });
    expect(inputsText({ a: "1", b: "2" })).toBe("a=1\nb=2");
    expect(inputsText(undefined)).toBe("");
  });
});
