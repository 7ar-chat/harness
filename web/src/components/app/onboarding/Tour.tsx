// A guided tour (coach marks) of the Console shell, with no dependency: a dimmed page with a cut-out around the part being explained and a
// small card beside it. Keyboard: Enter/Right for next, Left for back, Esc to leave; focus stays on the card and returns where it was.
import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { Button } from "@/components/ui/button";

export interface TourStep {
  id: string;
  /** CSS selector of the part to point at; without a match (or a visible one) the card is centered. */
  target?: string;
  title: string;
  text: string;
  /** Called when the step is shown, to make its target exist (open the panel, say). */
  before?: () => void;
}

export const SHELL_TOUR: Omit<TourStep, "before">[] = [
  { id: "explorer", target: 'aside[aria-label="Explorer panel"]', title: "The explorer", text: "Your projects, sessions and pages. The Needs you list at the top shows where agents are waiting for you." },
  { id: "tabs", target: '[role="tablist"][aria-label="Open editors"]', title: "Editor tabs", text: "Everything you open stays here as a tab: sessions, the board, tasks, settings. Switch like browser tabs." },
  { id: "needs", target: 'section[aria-label="Panel"]', title: "Needs you", text: "The bottom panel collects what agents ask of you: approve an action, answer a question, review a result. It opens by itself when something waits." },
  { id: "status", target: 'footer[aria-label="Status bar"]', title: "The status bar", text: "The repository, running sessions and what you spent this month against your cap. Its buttons show or hide the explorer and the panel." },
  { id: "palette", title: "Press Ctrl/Cmd + K", text: "The command palette jumps to any session, tab or project and runs actions. If you are lost, start there. Press ? any time for all shortcuts." },
];

interface Rect {
  top: number;
  left: number;
  width: number;
  height: number;
}

const visibleRect = (selector?: string): Rect | null => {
  if (!selector) return null;
  const el = document.querySelector(selector);
  if (!el) return null;
  const r = el.getBoundingClientRect();
  if (r.width < 2 || r.height < 2) return null;
  return { top: r.top, left: r.left, width: r.width, height: r.height };
};

export function Tour({ open, steps = SHELL_TOUR, onClose }: { open: boolean; steps?: TourStep[]; onClose: (completed: boolean) => void }) {
  const [i, setI] = useState(0);
  const [rect, setRect] = useState<Rect | null>(null);
  const card = useRef<HTMLDivElement>(null);
  const returnTo = useRef<Element | null>(null);
  const step = steps[i];

  useEffect(() => {
    if (open) {
      setI(0);
      returnTo.current = document.activeElement;
    }
  }, [open]);

  // make the target exist, then follow it (the layout can move: resizing, the panel opening)
  useLayoutEffect(() => {
    if (!open || !step) return;
    step.before?.();
    const measure = () => setRect(visibleRect(step.target));
    measure();
    const t = window.setInterval(measure, 250);
    window.addEventListener("resize", measure);
    return () => {
      window.clearInterval(t);
      window.removeEventListener("resize", measure);
    };
  }, [open, step]);

  useEffect(() => {
    if (open) card.current?.querySelector<HTMLElement>("[data-autofocus]")?.focus();
  }, [open, i]);

  const finish = (completed: boolean) => {
    onClose(completed);
    const el = returnTo.current;
    if (el instanceof HTMLElement) window.setTimeout(() => el.focus(), 0);
  };

  useEffect(() => {
    if (!open) return;
    const h = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.preventDefault();
        e.stopPropagation();
        finish(false);
      } else if (e.key === "ArrowRight") {
        e.preventDefault();
        setI((n) => Math.min(n + 1, steps.length - 1));
      } else if (e.key === "ArrowLeft") {
        e.preventDefault();
        setI((n) => Math.max(n - 1, 0));
      } else if (e.key === "Tab" && card.current) {
        // keep focus inside the card
        const f = [...card.current.querySelectorAll<HTMLElement>("button")];
        if (!f.length) return;
        const first = f[0]!;
        const last = f[f.length - 1]!;
        if (e.shiftKey && document.activeElement === first) (e.preventDefault(), last.focus());
        else if (!e.shiftKey && document.activeElement === last) (e.preventDefault(), first.focus());
        else if (!card.current.contains(document.activeElement)) (e.preventDefault(), first.focus());
      }
    };
    window.addEventListener("keydown", h, true);
    return () => window.removeEventListener("keydown", h, true);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, steps.length]);

  if (!open || !step) return null;
  const last = i === steps.length - 1;
  const pad = 6;
  const vw = window.innerWidth;
  const vh = window.innerHeight;
  const W = Math.min(340, vw - 24);
  let style: React.CSSProperties;
  if (rect) {
    // beside the target when it is tall and narrow, above or below when it is wide
    const roomRight = vw - (rect.left + rect.width);
    const left = Math.max(12, Math.min(vw - W - 12, roomRight > W + 20 ? rect.left + rect.width + 14 : rect.left + 12));
    const wide = roomRight <= W + 20;
    const top = wide ? (rect.top > vh / 2 ? Math.max(12, rect.top - 190) : Math.min(vh - 200, rect.top + rect.height + 14)) : Math.max(12, Math.min(vh - 220, rect.top + 12));
    style = { left, top, width: W };
  } else {
    style = { left: Math.max(12, (vw - W) / 2), top: Math.max(12, vh / 2 - 110), width: W };
  }
  return (
    <div className="fixed inset-0 z-[100]" data-testid="tour">
      {/* the dimmed page; the cut-out is a shadow around the target */}
      {rect ? (
        <div aria-hidden className="pointer-events-none absolute rounded-md ring-2 ring-primary transition-all" style={{ top: rect.top - pad, left: rect.left - pad, width: rect.width + pad * 2, height: rect.height + pad * 2, boxShadow: "0 0 0 9999px rgba(0,0,0,0.55)" }} />
      ) : (
        <div aria-hidden className="absolute inset-0 bg-black/55" />
      )}
      <div ref={card} role="dialog" aria-modal="true" aria-labelledby="tour-title" aria-describedby="tour-text" style={style} className="absolute space-y-3 rounded-lg border bg-popover p-4 text-popover-foreground shadow-lg">
        <div className="text-xs text-muted-foreground">
          Step {i + 1} of {steps.length}
        </div>
        <h2 id="tour-title" className="text-base font-semibold">
          {step.title}
        </h2>
        <p id="tour-text" className="text-sm text-muted-foreground">
          {step.text}
        </p>
        <div className="flex items-center justify-between gap-2">
          <Button variant="ghost" size="sm" onClick={() => finish(false)}>
            Skip tour
          </Button>
          <div className="flex gap-2">
            {i > 0 ? (
              <Button variant="outline" size="sm" onClick={() => setI(i - 1)}>
                Back
              </Button>
            ) : null}
            <Button size="sm" data-autofocus onClick={() => (last ? finish(true) : setI(i + 1))}>
              {last ? "Done" : "Next"}
            </Button>
          </div>
        </div>
      </div>
    </div>
  );
}
