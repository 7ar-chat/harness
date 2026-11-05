// The project's board: every session as a card in Ready, Running, Needs you, In review or Done.
import { Link } from "@tanstack/react-router";
import { formatDistanceToNowStrict } from "date-fns";
import { ExternalLink, GitPullRequest } from "lucide-react";
import { useBoard, useSeen, type BoardCard, type BoardColumn } from "@/api/queries";
import { Empty, ErrorState, ListSkeleton, StatusDot } from "@/components/app/common";
import { cn } from "@/lib/utils";
import { isUnread } from "@/lib/unread";

const ago = (iso: string) => formatDistanceToNowStrict(new Date(iso), { addSuffix: true });

const ACCENT: Record<BoardColumn["id"], string> = {
  ready: "bg-muted-foreground/40",
  running: "bg-orange-500",
  needs_you: "bg-red-500",
  review: "bg-blue-500",
  done: "bg-green-500",
};

function Card({ card }: { card: BoardCard }) {
  const seen = useSeen();
  const unread = isUnread(seen, card.threadId, card.updatedAt);
  return (
    <li>
      <Link
        to="/t/$threadId"
        params={{ threadId: card.threadId }}
        className="block rounded-lg border bg-card p-3 text-sm shadow-xs transition-colors hover:bg-accent"
      >
        <div className="flex items-start gap-2">
          <StatusDot status={card.status} unread={unread} className="mt-1.5" />
          <span className={cn("min-w-0 flex-1 break-words", unread && "font-semibold")}>{card.title || "(untitled)"}</span>
        </div>
        {card.why ? <p className="mt-1.5 line-clamp-3 pl-4 text-xs text-muted-foreground">{card.why}</p> : null}
        <div className="mt-2 flex items-center gap-2 pl-4 text-xs text-muted-foreground">
          {card.prNumber ? (
            <span className="inline-flex items-center gap-1">
              <GitPullRequest className="size-3" /> #{card.prNumber}
              {card.prUrl ? (
                <a href={card.prUrl} target="_blank" rel="noreferrer" aria-label={`Open pull request #${card.prNumber} on GitHub`} onClick={(e) => e.stopPropagation()} className="hover:text-foreground">
                  <ExternalLink className="size-3" />
                </a>
              ) : null}
            </span>
          ) : null}
          <span className="ml-auto">{ago(card.updatedAt)}</span>
        </div>
      </Link>
    </li>
  );
}

export function Board({ projectId }: { projectId: string }) {
  const board = useBoard(projectId);
  if (board.isLoading) return <ListSkeleton rows={3} />;
  if (board.error || !board.data) return <ErrorState error={board.error} retry={() => void board.refetch()} />;
  const cols = board.data.columns;
  if (cols.every((c) => c.cards.length === 0)) return <Empty title="No sessions yet">Start a session with the + next to the project; it shows up here as it works.</Empty>;
  return (
    <div className="grid gap-3 md:grid-cols-5" aria-label="Board">
      {cols.map((col) => (
        <section key={col.id} aria-label={col.title} className="min-w-0 rounded-lg bg-muted/40 p-2">
          <h3 className="mb-2 flex items-center gap-2 px-1 text-xs font-medium text-muted-foreground">
            <span className={cn("size-2 rounded-full", ACCENT[col.id])} />
            {col.title}
            <span className="ml-auto tabular-nums">{col.cards.length}</span>
          </h3>
          <ul className="space-y-2">
            {col.cards.map((c) => (
              <Card key={c.threadId} card={c} />
            ))}
          </ul>
        </section>
      ))}
    </div>
  );
}
