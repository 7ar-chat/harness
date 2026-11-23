// The canvas of a workflow: steps as boxes on an SVG, the dependencies as curves. The designer edits through it (drag a box to
// move it, click the dot on its right and then another box to connect them); the run view draws the same graph read-only with the
// state of each step.
import { useRef, useState } from "react";
import type { Node } from "./graph";
import { NODE_H, NODE_W, edgePath, edgesOf, extent, kindLabel, reworkEdgesOf, reworkPath } from "./graph";
import { cn } from "@/lib/utils";

/** How a step is painted in a run: its state decides the colour. */
const STATE_STROKE: Record<string, string> = {
  pending: "stroke-border",
  running: "stroke-primary",
  waiting: "stroke-warning",
  done: "stroke-success",
  failed: "stroke-destructive",
  skipped: "stroke-border",
  cancelled: "stroke-border",
};
export const STEP_STATE_LABEL: Record<string, string> = {
  pending: "Waiting",
  running: "Running",
  waiting: "Needs a decision",
  done: "Done",
  failed: "Failed",
  skipped: "Skipped",
  cancelled: "Cancelled",
};

const clip = (s: string, n: number) => (s.length > n ? s.slice(0, n - 1) + "…" : s);

/** The second line of a box: what the step is. */
function caption(n: Node): string {
  switch (n.kind) {
    case "agent":
      return n.role || "coder";
    case "gate":
      return (n.approvers ?? []).join(", ") || "no approver";
    case "subworkflow":
      return n.workflowId ? "another workflow" : "no workflow";
    default:
      return n.command ? clip(n.command, 26) : "no command";
  }
}

export interface CanvasProps {
  nodes: Node[];
  selected?: string | null;
  /** The step a connection starts from (designer only). */
  connecting?: string | null;
  /** Run view: the state of each step by node id. */
  states?: Record<string, string>;
  /** A step waiting for a decision gets a badge. */
  readOnly?: boolean;
  onSelect?: (id: string | null) => void;
  onMove?: (id: string, x: number, y: number) => void;
  onStartConnect?: (id: string) => void;
  onConnect?: (from: string, to: string) => void;
  selectedEdge?: { from: string; to: string } | null;
  onSelectEdge?: (e: { from: string; to: string } | null) => void;
  className?: string;
}

export function Canvas({ nodes, selected, connecting, states, readOnly, onSelect, onMove, onStartConnect, onConnect, selectedEdge, onSelectEdge, className }: CanvasProps) {
  const svg = useRef<SVGSVGElement>(null);
  const drag = useRef<{ id: string; dx: number; dy: number; moved: boolean } | null>(null);
  const [live, setLive] = useState<Record<string, { x: number; y: number }>>({});
  const by = new Map(nodes.map((n) => [n.id, { ...n, pos: live[n.id] ?? n.pos }]));
  const drawn = [...by.values()];
  const { w, h } = extent(drawn);

  const point = (e: React.PointerEvent) => {
    const r = svg.current?.getBoundingClientRect();
    const scale = r && r.width ? w / r.width : 1;
    return { x: (e.clientX - (r?.left ?? 0)) * scale, y: (e.clientY - (r?.top ?? 0)) * scale };
  };

  const down = (e: React.PointerEvent, n: Node) => {
    if (readOnly || !onMove) return;
    const p = point(e);
    drag.current = { id: n.id, dx: p.x - n.pos.x, dy: p.y - n.pos.y, moved: false };
    (e.currentTarget as Element).setPointerCapture?.(e.pointerId);
  };
  const move = (e: React.PointerEvent) => {
    const d = drag.current;
    if (!d) return;
    const p = point(e);
    d.moved = true;
    setLive((l) => ({ ...l, [d.id]: { x: Math.max(0, p.x - d.dx), y: Math.max(0, p.y - d.dy) } }));
  };
  const up = () => {
    const d = drag.current;
    drag.current = null;
    const at = d ? live[d.id] : undefined;
    if (d?.moved && at) onMove?.(d.id, at.x, at.y);
    setLive({});
  };
  const click = (n: Node) => {
    if (connecting && connecting !== n.id) onConnect?.(connecting, n.id);
    else onSelect?.(n.id);
  };

  return (
    <div className={cn("overflow-auto rounded-lg border bg-muted/20", className)}>
      <svg ref={svg} width={w} height={h} viewBox={`0 0 ${w} ${h}`} role="group" aria-label="Workflow canvas" onPointerMove={move} onPointerUp={up} onClick={(e) => e.target === e.currentTarget && (onSelect?.(null), onSelectEdge?.(null))}>
        <defs>
          <marker id="wf-arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
            <path d="M 0 0 L 10 5 L 0 10 z" className="fill-muted-foreground" />
          </marker>
        </defs>
        {edgesOf(drawn).map((e) => {
          const a = by.get(e.from)!;
          const b = by.get(e.to)!;
          const sel = selectedEdge?.from === e.from && selectedEdge?.to === e.to;
          return (
            <g key={`${e.from}>${e.to}`}>
              <path d={edgePath(a, b)} fill="none" className={sel ? "stroke-primary" : "stroke-muted-foreground"} strokeWidth={sel ? 2.5 : 1.5} markerEnd="url(#wf-arrow)" />
              {onSelectEdge ? <path d={edgePath(a, b)} fill="none" stroke="transparent" strokeWidth={14} role="button" aria-label={`Connection from ${e.from} to ${e.to}`} className="cursor-pointer" onClick={() => onSelectEdge(e)} /> : null}
            </g>
          );
        })}
        {reworkEdgesOf(drawn).map((e) => (
          <path key={`r${e.from}>${e.to}`} d={reworkPath(by.get(e.from)!, by.get(e.to)!)} fill="none" className="stroke-warning" strokeWidth={1.5} strokeDasharray="5 4" markerEnd="url(#wf-arrow)">
            <title>{`${e.from} goes back to ${e.to} on failure`}</title>
          </path>
        ))}
        {drawn.map((n) => {
          const st = states?.[n.id];
          const sel = selected === n.id;
          return (
            <g
              key={n.id}
              transform={`translate(${n.pos.x} ${n.pos.y})`}
              role="button"
              tabIndex={0}
              aria-label={`Step ${n.title || n.id}`}
              aria-pressed={sel}
              data-kind={n.kind}
              data-state={st}
              className={cn("outline-none", readOnly ? "cursor-pointer" : "cursor-grab")}
              onPointerDown={(e) => down(e, n)}
              onClick={(e) => (e.stopPropagation(), click(n))}
              onKeyDown={(e) => {
                if (e.key === "Enter" || e.key === " ") {
                  e.preventDefault();
                  click(n);
                }
              }}
            >
              <rect width={NODE_W} height={NODE_H} rx={10} className={cn("fill-card", st ? STATE_STROKE[st] : "stroke-border", sel && "stroke-primary", connecting && connecting !== n.id && "stroke-primary/60")} strokeWidth={sel || st === "running" || st === "waiting" ? 2.5 : 1.5} />
              <text x={12} y={24} className="fill-foreground text-[13px] font-medium">
                {clip(n.title || n.id, 22)}
              </text>
              <text x={12} y={44} className="fill-muted-foreground text-[11px]">
                {kindLabel(n.kind)} · {caption(n)}
              </text>
              {st ? (
                <text x={NODE_W - 10} y={14} textAnchor="end" className={cn("text-[10px]", st === "failed" ? "fill-destructive" : st === "waiting" ? "fill-warning" : "fill-muted-foreground")}>
                  {STEP_STATE_LABEL[st] ?? st}
                </text>
              ) : null}
              {!readOnly && onStartConnect ? (
                <circle
                  cx={NODE_W}
                  cy={NODE_H / 2}
                  r={7}
                  role="button"
                  tabIndex={0}
                  aria-label={`Connect from ${n.id}`}
                  className={cn("cursor-crosshair stroke-primary", connecting === n.id ? "fill-primary" : "fill-background")}
                  strokeWidth={2}
                  onPointerDown={(e) => e.stopPropagation()}
                  onClick={(e) => (e.stopPropagation(), onStartConnect(n.id))}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" || e.key === " ") {
                      e.preventDefault();
                      e.stopPropagation();
                      onStartConnect(n.id);
                    }
                  }}
                />
              ) : null}
            </g>
          );
        })}
      </svg>
    </div>
  );
}
