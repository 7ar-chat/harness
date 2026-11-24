// A collapsed "Advanced" section for forms: a real disclosure button, its open state remembered per browser, opened on its own
// when a field inside holds a non-default value or a validation problem sits inside it. Content is not rendered while closed.
import { ChevronRight } from "lucide-react";
import { useEffect, useId, useState } from "react";
import { cn } from "@/lib/utils";

const read = (key: string): boolean => {
  try {
    return localStorage.getItem(key) === "1";
  } catch {
    return false;
  }
};
const write = (key: string, on: boolean) => {
  try {
    localStorage.setItem(key, on ? "1" : "0");
  } catch {
    /* storage can be unavailable: the choice then lasts for this dialog only */
  }
};

export function Advanced({ storageKey, nonDefault, invalid, hint, children }: { storageKey: string; nonDefault?: boolean; invalid?: boolean; hint?: string; children: React.ReactNode }) {
  const id = useId();
  const [open, setOpen] = useState(() => read(storageKey) || !!nonDefault || !!invalid);
  // A value that becomes non-default (a template filled the flags) or a problem inside the section opens it.
  useEffect(() => {
    if (nonDefault || invalid) setOpen(true);
  }, [nonDefault, invalid]);
  const toggle = () => {
    setOpen(!open);
    write(storageKey, !open);
  };
  return (
    <section className="rounded-md border" aria-label="Advanced settings">
      <button type="button" aria-expanded={open} aria-controls={id} onClick={toggle} className="flex w-full items-center gap-2 rounded-md px-3 py-2 text-left text-sm font-medium hover:bg-accent">
        <ChevronRight className={cn("size-4 shrink-0 transition-transform", open && "rotate-90")} aria-hidden />
        Advanced
        {hint ? <span className="min-w-0 truncate text-xs font-normal text-muted-foreground">{hint}</span> : null}
      </button>
      <div id={id} hidden={!open} className="space-y-4 border-t p-3">
        {open ? children : null}
      </div>
    </section>
  );
}
