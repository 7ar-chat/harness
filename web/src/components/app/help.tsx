// Help in context: the article renderer, a "?" button that opens an article in a side sheet, and a "Learn more" link.
import { Link, useRouter } from "@tanstack/react-router";
import { CircleHelp } from "lucide-react";
import { useEffect, useState, type ReactNode } from "react";
import Markdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { articleBySlug, helpHref } from "@/lib/help";
import { cn } from "@/lib/utils";

/**
 * Escape closes just the topmost sheet. Over a form dialog (a new task) it must leave the form, with what was typed, alone, so the key is
 * taken here, before it reaches the dialog's own handler.
 */
export function useEscapeClose(open: boolean, close: () => void) {
  useEffect(() => {
    if (!open) return;
    const h = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      e.stopPropagation();
      e.preventDefault();
      close();
    };
    window.addEventListener("keydown", h, true);
    return () => window.removeEventListener("keydown", h, true);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);
}

/** An article's body. A link written as (help:slug) goes to another article through `onArticle`. */
export function HelpMarkdown({ body, onArticle }: { body: string; onArticle: (slug: string) => void }) {
  return (
    <div className="space-y-3 text-sm leading-relaxed [&_code]:rounded [&_code]:bg-muted [&_code]:px-1 [&_code]:py-0.5 [&_code]:font-mono [&_code]:text-xs [&_h2]:pt-2 [&_h2]:text-base [&_h2]:font-semibold [&_li]:ml-5 [&_li]:list-disc [&_ol_li]:list-decimal [&_strong]:font-semibold">
      <Markdown
        remarkPlugins={[remarkGfm]}
        urlTransform={(u) => u}
        components={{
          a: ({ href, children }) =>
            href?.startsWith("help:") ? (
              <button type="button" className="text-primary underline underline-offset-2" onClick={() => onArticle(href.slice(5))}>
                {children}
              </button>
            ) : (
              <a href={href} target="_blank" rel="noreferrer" className="text-primary underline underline-offset-2">
                {children}
              </a>
            ),
          table: ({ children }) => (
            <div className="overflow-x-auto rounded-md border">
              <table className="w-full text-left text-xs">{children}</table>
            </div>
          ),
          th: ({ children }) => <th className="border-b bg-muted/50 px-2 py-1.5 font-medium">{children}</th>,
          td: ({ children }) => <td className="border-b px-2 py-1.5 align-top last:border-b-0">{children}</td>,
        }}
      >
        {body}
      </Markdown>
    </div>
  );
}

/** A small "?" next to a heading: opens the article in a side sheet, so nobody has to leave what they are doing. */
export function HelpButton({ slug, className, label }: { slug: string; className?: string; label?: string }) {
  const [open, setOpen] = useState(false);
  const [current, setCurrent] = useState(slug);
  const router = useRouter();
  const article = articleBySlug(current);
  useEscapeClose(open, () => setOpen(false));
  if (!articleBySlug(slug)) return null;
  const name = `${label ?? articleBySlug(slug)!.title}`;
  return (
    <>
      <button
        type="button"
        onClick={() => {
          setCurrent(slug);
          setOpen(true);
        }}
        aria-label={`${name} (help)`}
        title={`Help: ${name}`}
        className={cn("inline-flex size-5 shrink-0 items-center justify-center rounded-full text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring", className)}
      >
        <CircleHelp className="size-4" aria-hidden />
      </button>
      <Sheet open={open} onOpenChange={setOpen}>
        <SheetContent
          side="right"
          className="w-[420px] max-w-[92vw] gap-0 overflow-y-auto sm:max-w-[420px]"
        >
          <SheetHeader>
            <SheetTitle>{article?.title}</SheetTitle>
            <SheetDescription>{article?.summary}</SheetDescription>
          </SheetHeader>
          <div className="space-y-4 px-4 pb-6">
            {article ? <HelpMarkdown body={article.body} onArticle={(s) => articleBySlug(s) && setCurrent(s)} /> : null}
            <button
              type="button"
              className="text-sm text-primary underline underline-offset-2"
              onClick={() => {
                setOpen(false);
                void router.navigate({ href: helpHref(current) });
              }}
            >
              Open in the help center
            </button>
          </div>
        </SheetContent>
      </Sheet>
    </>
  );
}

/** A heading with its "?" beside it. */
export function HeadingWithHelp({ slug, children, className }: { slug: string; children: ReactNode; className?: string }) {
  return (
    <span className={cn("inline-flex items-center gap-1.5", className)}>
      {children}
      <HelpButton slug={slug} />
    </span>
  );
}

/** "Learn more" link to an article of the help center. */
export function LearnMore({ slug, className }: { slug: string; className?: string }) {
  return (
    <Link to="/help" search={{ a: slug }} className={cn("text-sm text-primary underline underline-offset-2", className)}>
      Learn more
    </Link>
  );
}
