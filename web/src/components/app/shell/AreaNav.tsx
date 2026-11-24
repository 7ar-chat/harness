// The sub-navigation of an area: its views as a row of pill tabs at the top of the page. A real tablist: one tab stops (the shown one), arrow
// keys, Home and End move between views (and show them), the shown view is aria-selected. The row scrolls sideways on a phone, never the page.
import { useRouter } from "@tanstack/react-router";
import { useEffect, useId, useRef, type ReactNode } from "react";
import { cn } from "@/lib/utils";
import { rememberView, viewHref, visibleViews, type Area } from "./areas";

export function AreaNav({ area, current, projectId, owner = true, navClassName, panelClassName = "space-y-4", children }: { area: Area; current: string; projectId?: string; owner?: boolean; navClassName?: string; panelClassName?: string; children: ReactNode }) {
  const router = useRouter();
  const uid = useId();
  const views = visibleViews(area, { owner, current });
  const list = useRef<HTMLDivElement>(null);
  useEffect(() => rememberView(area, current, projectId), [area, current, projectId]);
  const show = (i: number) => {
    const v = views[(i + views.length) % views.length]!;
    void router.navigate({ href: viewHref(area, v, projectId) });
    // the tab keeps its place in the row; focus follows the selection
    requestAnimationFrame(() => list.current?.querySelector<HTMLElement>(`[data-view="${CSS.escape(v.id)}"]`)?.focus());
  };
  const tabId = (id: string) => `${uid}-tab-${id}`;
  return (
    <>
      <div className={cn("-mx-1 overflow-x-auto px-1 pb-1", navClassName)}>
        <div ref={list} role="tablist" aria-label={`${area.label} views`} className="inline-flex w-max gap-1 rounded-lg bg-muted p-1">
          {views.map((v, i) => {
            const on = v.id === current;
            return (
              <button
                key={v.id}
                type="button"
                role="tab"
                id={tabId(v.id)}
                data-view={v.id}
                aria-selected={on}
                aria-controls={`${uid}-panel`}
                tabIndex={on ? 0 : -1}
                onClick={() => !on && show(i)}
                onKeyDown={(e) => {
                  if (e.key === "ArrowRight") show(i + 1);
                  else if (e.key === "ArrowLeft") show(i - 1);
                  else if (e.key === "Home") show(0);
                  else if (e.key === "End") show(views.length - 1);
                  else return;
                  e.preventDefault();
                }}
                className={cn(
                  "shrink-0 whitespace-nowrap rounded-md px-3 py-1 text-[13px] font-medium outline-none focus-visible:ring-2 focus-visible:ring-ring",
                  on ? "bg-background text-foreground shadow-sm" : "text-muted-foreground hover:text-foreground",
                )}
              >
                {v.label}
              </button>
            );
          })}
        </div>
      </div>
      <div role="tabpanel" id={`${uid}-panel`} aria-labelledby={tabId(current)} className={panelClassName}>
        {children}
      </div>
    </>
  );
}
