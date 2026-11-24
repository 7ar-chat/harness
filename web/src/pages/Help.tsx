// The help center: a searchable list of short articles. /help?a=<slug> opens one.
import { useLocation, useRouter } from "@tanstack/react-router";
import { ArrowLeft, Search } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { HelpMarkdown } from "@/components/app/help";
import { Input } from "@/components/ui/input";
import { ScrollArea } from "@/components/ui/scroll-area";
import { ARTICLES, GROUPS, articleBySlug, helpHref, searchArticles } from "@/lib/help";
import { requestOnboarding } from "@/lib/onboarding";

export function HelpPage() {
  const router = useRouter();
  const active = useLocation({ select: (l) => l.pathname === "/help" });
  const fromAddress = useLocation({ select: (l) => (l.pathname === "/help" ? (l.search as { a?: string }).a : undefined) });
  const [slug, setSlug] = useState<string | undefined>(articleBySlug(fromAddress) ? fromAddress : undefined);
  const [q, setQ] = useState("");
  // a deep link (or a "Learn more") while this tab is already open
  useEffect(() => {
    if (active) setSlug(articleBySlug(fromAddress) ? fromAddress : undefined);
  }, [active, fromAddress]);
  const open = (s?: string) => {
    setSlug(s);
    if (active) void router.navigate({ href: helpHref(s), replace: true });
  };
  const results = useMemo(() => searchArticles(q), [q]);
  const article = articleBySlug(slug);
  return (
    <ScrollArea className="h-full">
      <div className="mx-auto w-full max-w-3xl space-y-5 p-4 md:p-8">
        <header className="space-y-1">
          <h1 className="text-xl font-semibold">Help</h1>
          <p className="text-sm text-muted-foreground">Short answers in plain language. Nothing here needs the internet.</p>
        </header>
        {article ? (
          <article aria-labelledby="help-article-h" className="space-y-4">
            <button type="button" onClick={() => open(undefined)} className="inline-flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground">
              <ArrowLeft className="size-4" /> All articles
            </button>
            <div className="space-y-1">
              <h2 id="help-article-h" className="text-lg font-semibold">
                {article.title}
              </h2>
              <p className="text-sm text-muted-foreground">{article.summary}</p>
            </div>
            <HelpMarkdown body={article.body} onArticle={(s) => articleBySlug(s) && open(s)} />
          </article>
        ) : (
          <>
            <div className="relative">
              <Search className="absolute left-2.5 top-2.5 size-4 text-muted-foreground" aria-hidden />
              <Input value={q} onChange={(e) => setQ(e.target.value)} placeholder="Search the help" aria-label="Search the help" className="pl-8" type="search" />
            </div>
            <div className="flex flex-wrap gap-2 text-sm">
              <button type="button" className="rounded-md border px-2.5 py-1 hover:bg-accent" onClick={() => requestOnboarding("welcome")}>
                Replay the welcome
              </button>
              <button type="button" className="rounded-md border px-2.5 py-1 hover:bg-accent" onClick={() => requestOnboarding("tour")}>
                Take the tour
              </button>
              <button type="button" className="rounded-md border px-2.5 py-1 hover:bg-accent" onClick={() => requestOnboarding("shortcuts")}>
                Keyboard shortcuts
              </button>
            </div>
            {results.length === 0 ? (
              <p className="text-sm text-muted-foreground" role="status">
                No article matches “{q}”. Try a shorter word, such as “task” or “budget”.
              </p>
            ) : q.trim() ? (
              <ul className="space-y-2" aria-label="Search results">
                {results.map((a) => (
                  <ArticleRow key={a.slug} slug={a.slug} onOpen={open} />
                ))}
              </ul>
            ) : (
              GROUPS.map((g) => (
                <section key={g} aria-label={g} className="space-y-2">
                  <h2 className="text-sm font-medium text-muted-foreground">{g}</h2>
                  <ul className="space-y-2">
                    {ARTICLES.filter((a) => a.group === g).map((a) => (
                      <ArticleRow key={a.slug} slug={a.slug} onOpen={open} />
                    ))}
                  </ul>
                </section>
              ))
            )}
          </>
        )}
      </div>
    </ScrollArea>
  );
}

function ArticleRow({ slug, onOpen }: { slug: string; onOpen: (s: string) => void }) {
  const a = articleBySlug(slug)!;
  return (
    <li>
      <button type="button" onClick={() => onOpen(a.slug)} className="block w-full rounded-lg border bg-card p-3 text-left transition-colors hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring">
        <div className="text-sm font-medium">{a.title}</div>
        <div className="text-xs text-muted-foreground">{a.summary}</div>
      </button>
    </li>
  );
}
