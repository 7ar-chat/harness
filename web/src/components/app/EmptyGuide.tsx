// The empty state that teaches: what this place is (one sentence), the main thing to do here, and a way to read more.
import type { ReactNode } from "react";
import { LearnMore } from "./help";

export function EmptyGuide({ icon, title, what, children, slug }: { icon?: ReactNode; title: string; /** one plain sentence: what this is for */ what: ReactNode; /** the primary action(s) */ children?: ReactNode; /** help article for "Learn more" */ slug?: string }) {
  return (
    <section aria-label={title} className="flex flex-col items-center justify-center gap-3 p-8 text-center" data-testid="empty-guide">
      {icon ? <div className="text-muted-foreground">{icon}</div> : null}
      <h3 className="text-sm font-medium text-foreground">{title}</h3>
      <p className="max-w-sm text-sm text-muted-foreground">{what}</p>
      {children ? <div className="mt-1 flex flex-wrap items-center justify-center gap-2">{children}</div> : null}
      {slug ? <LearnMore slug={slug} /> : null}
    </section>
  );
}
