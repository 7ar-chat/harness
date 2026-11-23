// The logic of the workflow designer, apart from the canvas: editing a graph of steps (add, connect, remove, rename),
// the checks the editor can make while typing (the server checks again on save), the layout and the drawing of edges.
import type { WorkflowNode } from "@/api/types";

export type Kind = WorkflowNode["kind"];
export type Node = WorkflowNode;

export const NODE_W = 184;
export const NODE_H = 64;
const GRID = 10;

export const KINDS: { kind: Kind; label: string; hint: string }[] = [
  { kind: "agent", label: "Agent", hint: "An agent with a role does the step" },
  { kind: "script", label: "Script", hint: "A shell command that must exit 0" },
  { kind: "gate", label: "Gate", hint: "A person approves, sends back or adds context" },
  { kind: "subworkflow", label: "Workflow", hint: "Runs another workflow of the project" },
  { kind: "build", label: "Build", hint: "A build command that must exit 0" },
  { kind: "preview", label: "Preview", hint: "A command that prepares a preview and must exit 0" },
];
export const kindLabel = (k: Kind) => KINDS.find((x) => x.kind === k)?.label ?? k;

export const ID_RE = /^[a-z][a-z0-9_-]{0,31}$/;
export const isCommand = (k: Kind) => k === "script" || k === "build" || k === "preview";

const snap = (n: number) => Math.max(0, Math.round(n / GRID) * GRID);

/** An id for a new node: the kind's word, then a number when it is taken. */
export function freeId(nodes: Node[], base: string): string {
  const taken = new Set(nodes.map((n) => n.id));
  if (!taken.has(base)) return base;
  for (let i = 2; ; i++) if (!taken.has(`${base}${i}`)) return `${base}${i}`;
}

/** A node of a kind with the fields that kind needs, so it can be saved as it is. */
export function newNode(kind: Kind, nodes: Node[], pos: { x: number; y: number }): Node {
  const id = freeId(nodes, kind === "subworkflow" ? "workflow" : kind);
  const n: Node = { id, kind, title: kindLabel(kind), pos: { x: snap(pos.x), y: snap(pos.y) } };
  if (kind === "agent") {
    n.role = "coder";
    n.prompt = "";
  }
  if (isCommand(kind)) n.command = "";
  if (kind === "gate") n.approvers = ["role:member"];
  return n;
}

/** Where a node added next goes: right of the rightmost node, or the origin. */
export function nextPos(nodes: Node[]): { x: number; y: number } {
  if (!nodes.length) return { x: 40, y: 40 };
  const last = nodes.reduce((a, b) => (b.pos.x >= a.pos.x ? b : a));
  return { x: last.pos.x + NODE_W + 60, y: last.pos.y };
}

/** Every id the node depends on, directly or not. */
export function ancestors(nodes: Node[], id: string): string[] {
  const by = new Map(nodes.map((n) => [n.id, n]));
  const seen = new Set<string>();
  const walk = (x: string) => {
    for (const d of by.get(x)?.dependsOn ?? []) {
      if (!seen.has(d)) {
        seen.add(d);
        walk(d);
      }
    }
  };
  walk(id);
  return [...seen];
}

/** Whether making `to` depend on `from` closes a loop (from already depends on to, or they are the same). */
export const wouldCycle = (nodes: Node[], from: string, to: string) => from === to || ancestors(nodes, from).includes(to);

/** Makes `to` wait for `from`. error says why a connection was refused. */
export function connect(nodes: Node[], from: string, to: string): { nodes: Node[]; error?: string } {
  if (from === to) return { nodes, error: "A step cannot wait for itself." };
  if (!nodes.some((n) => n.id === from) || !nodes.some((n) => n.id === to)) return { nodes, error: "Unknown step." };
  if (nodes.find((n) => n.id === to)?.dependsOn?.includes(from)) return { nodes, error: "These steps are already connected." };
  if (wouldCycle(nodes, from, to)) return { nodes, error: "That connection would make the steps wait for each other." };
  return { nodes: nodes.map((n) => (n.id === to ? { ...n, dependsOn: [...(n.dependsOn ?? []), from] } : n)) };
}

export const disconnect = (nodes: Node[], from: string, to: string): Node[] =>
  nodes.map((n) => (n.id === to ? { ...n, dependsOn: (n.dependsOn ?? []).filter((d) => d !== from) } : n));

/** Removes a node and every reference to it (edges, rework targets). */
export function removeNode(nodes: Node[], id: string): Node[] {
  return nodes
    .filter((n) => n.id !== id)
    .map((n) => {
      const next: Node = { ...n, dependsOn: (n.dependsOn ?? []).filter((d) => d !== id) };
      if (next.onFailure?.target === id) next.onFailure = { ...next.onFailure, target: "", action: next.onFailure.action === "rework" ? "fail" : next.onFailure.action };
      if (next.inputs && n.workflowId === id) next.inputs = undefined;
      return next;
    });
}

/** Renames a node and what refers to it, including {{nodes.old.…}} in prompts and commands. */
export function renameNode(nodes: Node[], from: string, to: string): { nodes: Node[]; error?: string } {
  if (from === to) return { nodes };
  if (!ID_RE.test(to)) return { nodes, error: "An id is a lowercase letter, then lowercase letters, digits, - or _ (32 at most)." };
  if (nodes.some((n) => n.id === to)) return { nodes, error: "Another step has this id." };
  const text = (s?: string) => s?.split(`{{nodes.${from}.`).join(`{{nodes.${to}.`);
  return {
    nodes: nodes.map((n) => {
      const next: Node = { ...n, id: n.id === from ? to : n.id, dependsOn: (n.dependsOn ?? []).map((d) => (d === from ? to : d)) };
      if (n.prompt) next.prompt = text(n.prompt);
      if (n.command) next.command = text(n.command);
      if (n.onFailure?.target === from) next.onFailure = { ...n.onFailure, target: to };
      return next;
    }),
  };
}

export interface Edge {
  from: string;
  to: string;
}
export const edgesOf = (nodes: Node[]): Edge[] => nodes.flatMap((n) => (n.dependsOn ?? []).map((d) => ({ from: d, to: n.id }))).filter((e) => nodes.some((n) => n.id === e.from));

/** The rework edges (a failure or a send-back goes back to an earlier step), drawn dashed. */
export const reworkEdgesOf = (nodes: Node[]): Edge[] =>
  nodes
    .filter((n) => n.onFailure?.action === "rework" || (n.kind === "gate" && n.onFailure?.target))
    .map((n) => ({ from: n.id, to: n.onFailure?.target || n.dependsOn?.[0] || "" }))
    .filter((e) => e.to && nodes.some((n) => n.id === e.to));

const centre = (n: Node) => ({ x: n.pos.x + NODE_W / 2, y: n.pos.y + NODE_H / 2 });

/** A smooth path from the right side of one node to the left side of the other. */
export function edgePath(a: Node, b: Node): string {
  const x1 = a.pos.x + NODE_W;
  const y1 = a.pos.y + NODE_H / 2;
  const x2 = b.pos.x;
  const y2 = b.pos.y + NODE_H / 2;
  const dx = Math.max(40, Math.abs(x2 - x1) / 2);
  return `M ${x1} ${y1} C ${x1 + dx} ${y1}, ${x2 - dx} ${y2}, ${x2} ${y2}`;
}

/** A path that goes over the top, from a node back to an earlier one (rework). */
export function reworkPath(a: Node, b: Node): string {
  const from = centre(a);
  const to = centre(b);
  const top = Math.min(a.pos.y, b.pos.y) - 36;
  return `M ${from.x} ${a.pos.y} C ${from.x} ${top}, ${to.x} ${top}, ${to.x} ${b.pos.y}`;
}

/** The size of the drawing: room for every node and a margin. */
export function extent(nodes: Node[]): { w: number; h: number } {
  return { w: Math.max(640, ...nodes.map((n) => n.pos.x + NODE_W + 80)), h: Math.max(320, ...nodes.map((n) => n.pos.y + NODE_H + 80)) };
}

/** Lays the nodes out in columns by depth (the longest chain of dependencies), rows in the order they appear. */
export function autoLayout(nodes: Node[]): Node[] {
  const depth = new Map<string, number>();
  const by = new Map(nodes.map((n) => [n.id, n]));
  const visiting = new Set<string>();
  const d = (id: string): number => {
    if (depth.has(id)) return depth.get(id)!;
    if (visiting.has(id)) return 0; // a cycle: the server will refuse it
    visiting.add(id);
    const deps = (by.get(id)?.dependsOn ?? []).filter((x) => by.has(x));
    const v = deps.length ? 1 + Math.max(...deps.map(d)) : 0;
    visiting.delete(id);
    depth.set(id, v);
    return v;
  };
  const rows = new Map<number, number>();
  return nodes.map((n) => {
    const col = d(n.id);
    const row = rows.get(col) ?? 0;
    rows.set(col, row + 1);
    return { ...n, pos: { x: 40 + col * (NODE_W + 60), y: 40 + row * (NODE_H + 50) } };
  });
}

export interface Issue {
  nodeId?: string;
  field?: string;
  message: string;
}

/** What can be checked without the server: the same rules as core/workflows' Validate, minus roles and subworkflows. */
export function localIssues(name: string, nodes: Node[], inputNames: string[] = []): Issue[] {
  const out: Issue[] = [];
  const add = (nodeId: string | undefined, field: string, message: string) => out.push({ nodeId, field, message });
  if (!name.trim()) add(undefined, "name", "The workflow needs a name.");
  if (!nodes.length) add(undefined, "nodes", "Add a first step.");
  const ids = new Map<string, number>();
  for (const n of nodes) ids.set(n.id, (ids.get(n.id) ?? 0) + 1);
  for (const n of nodes) {
    if (!ID_RE.test(n.id)) add(n.id, "id", "The id must be a lowercase slug.");
    if ((ids.get(n.id) ?? 0) > 1) add(n.id, "id", "Two steps have this id.");
    for (const d of n.dependsOn ?? []) if (!ids.has(d)) add(n.id, "dependsOn", `Waits for ${d}, which does not exist.`);
    if (n.kind === "agent" && !n.prompt?.trim()) add(n.id, "prompt", "The agent needs a prompt.");
    if (isCommand(n.kind) && !n.command?.trim()) add(n.id, "command", "The step needs a command.");
    if (n.kind === "gate") {
      if (!n.approvers?.length) add(n.id, "approvers", "A gate needs at least one approver.");
      if (!n.dependsOn?.length) add(n.id, "dependsOn", "A gate must follow a step: it has nothing to review.");
    }
    if (n.kind === "subworkflow" && !n.workflowId) add(n.id, "workflowId", "Choose the workflow to run.");
    if (n.onFailure?.action === "rework" && n.kind !== "gate" && !n.onFailure.target) add(n.id, "onFailure", "A rework edge needs the step to go back to.");
    for (const text of [n.prompt, n.command]) {
      for (const m of (text ?? "").matchAll(/\{\{\s*([A-Za-z0-9_.-]+)\s*\}\}/g)) {
        const parts = (m[1] ?? "").split(".");
        if (parts[0] === "inputs" && !inputNames.includes(parts[1] ?? "")) add(n.id, "prompt", `{{${m[1] ?? ""}}}: no such input.`);
        if (parts[0] === "nodes" && !ids.has(parts[1] ?? "")) add(n.id, "prompt", `{{${m[1] ?? ""}}}: no such step.`);
      }
    }
  }
  const cyc = cycleOf(nodes);
  if (cyc) add(undefined, "dependsOn", `The steps form a cycle: ${cyc}.`);
  return out;
}

/** "a -> b -> a" when the dependencies loop, else "". */
export function cycleOf(nodes: Node[]): string {
  const by = new Map(nodes.map((n) => [n.id, n]));
  const state = new Map<string, number>();
  const stack: string[] = [];
  let found = "";
  const visit = (id: string): boolean => {
    if (state.get(id) === 1) {
      found = [...stack.slice(stack.indexOf(id)), id].join(" -> ");
      return true;
    }
    if (state.get(id) === 2) return false;
    state.set(id, 1);
    stack.push(id);
    for (const d of by.get(id)?.dependsOn ?? []) if (by.has(d) && visit(d)) return true;
    stack.pop();
    state.set(id, 2);
    return false;
  };
  for (const n of nodes) if (visit(n.id)) return found;
  return "";
}

/** "a, b\nc" as a list without blanks or repeats. */
export const parseList = (s: string): string[] => [...new Set(s.split(/[\n,]/).map((x) => x.trim()).filter(Boolean))];

/** "key=value" lines as a map, and back. */
export const parseInputs = (s: string): Record<string, string> =>
  Object.fromEntries(
    s
      .split("\n")
      .map((l) => l.trim())
      .filter((l) => l.includes("="))
      .map((l) => [l.slice(0, l.indexOf("=")).trim(), l.slice(l.indexOf("=") + 1).trim()] as const)
      .filter(([k]) => k),
  );
export const inputsText = (m?: Record<string, string>) => Object.entries(m ?? {}).map(([k, v]) => `${k}=${v}`).join("\n");

/** Whether two graphs are the same, for the "unsaved changes" mark. */
export const sameGraph = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b);

/** A node dragged to a new place, snapped to the grid. */
export const moveNode = (nodes: Node[], id: string, x: number, y: number): Node[] => nodes.map((n) => (n.id === id ? { ...n, pos: { x: snap(x), y: snap(y) } } : n));
