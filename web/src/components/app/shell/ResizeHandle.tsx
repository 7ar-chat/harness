// A drag handle for the explorer's width and the bottom panel's height: pointer drag, arrow keys (Shift for bigger steps),
// Home/End for the limits and a double-click to reset. The caller keeps the size (and remembers it).
import { useRef } from "react";
import { cn } from "@/lib/utils";

export const clamp = (v: number, min: number, max: number) => Math.min(max, Math.max(min, v));

export function ResizeHandle({ orientation, value, min, max, initial, onChange, label, className }: { orientation: "vertical" | "horizontal"; value: number; min: number; max: number; initial: number; onChange: (v: number) => void; label: string; className?: string }) {
  // "vertical" separates left/right panes (drag changes the width); "horizontal" separates top/bottom (drag up grows the panel below).
  const drag = useRef<{ at: number; from: number } | null>(null);
  const axis = (e: { clientX: number; clientY: number }) => (orientation === "vertical" ? e.clientX : e.clientY);
  const sign = orientation === "vertical" ? 1 : -1;
  return (
    <div
      role="separator"
      aria-orientation={orientation === "vertical" ? "vertical" : "horizontal"}
      aria-label={label}
      aria-valuenow={Math.round(value)}
      aria-valuemin={min}
      aria-valuemax={max}
      tabIndex={0}
      title="Drag to resize, double-click to reset"
      onPointerDown={(e) => {
        e.currentTarget.setPointerCapture?.(e.pointerId);
        drag.current = { at: axis(e), from: value };
      }}
      onPointerMove={(e) => {
        if (drag.current) onChange(clamp(drag.current.from + sign * (axis(e) - drag.current.at), min, max));
      }}
      onPointerUp={(e) => {
        drag.current = null;
        e.currentTarget.releasePointerCapture?.(e.pointerId);
      }}
      onDoubleClick={() => onChange(initial)}
      onKeyDown={(e) => {
        const step = e.shiftKey ? 64 : 16;
        const grow = orientation === "vertical" ? "ArrowRight" : "ArrowUp";
        const shrink = orientation === "vertical" ? "ArrowLeft" : "ArrowDown";
        if (e.key === grow) onChange(clamp(value + step, min, max));
        else if (e.key === shrink) onChange(clamp(value - step, min, max));
        else if (e.key === "Home") onChange(min);
        else if (e.key === "End") onChange(max);
        else if (e.key === "Enter") onChange(initial);
        else return;
        e.preventDefault();
      }}
      className={cn(
        "group relative z-10 shrink-0 touch-none outline-none",
        orientation === "vertical" ? "-mx-[3px] w-[6px] cursor-col-resize" : "-my-[3px] h-[6px] cursor-row-resize",
        className,
      )}
    >
      <span className={cn("absolute bg-transparent transition-colors group-hover:bg-primary/60 group-focus-visible:bg-primary group-active:bg-primary", orientation === "vertical" ? "inset-y-0 left-[2px] w-[2px]" : "inset-x-0 top-[2px] h-[2px]")} />
    </div>
  );
}
