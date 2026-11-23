// The "Needs review" inbox: everything across projects that waits on the person.
import { Link } from "@tanstack/react-router";
import { formatDistanceToNowStrict } from "date-fns";
import { ExternalLink, GitPullRequest, Inbox as InboxIcon } from "lucide-react";
import { useState } from "react";
import { useNeedsReview } from "@/api/factory";
import type { InboxGate, InboxTask } from "@/api/types";
import { Empty, ErrorState, ListSkeleton } from "@/components/app/common";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { ScrollArea } from "@/components/ui/scroll-area";
import { GateActions } from "@/components/workflows/RunView";
import { SessionCard } from "./Dashboard";
import { ResumeForm } from "./Tasks";

const ago = (iso: string) => `${formatDistanceToNowStrict(new Date(iso))} ago`;

function TaskItem({ it }: { it: InboxTask }) {
  const [resuming, setResuming] = useState(false);
  const t = it.task;
  const blocked = it.kind === "blocked";
  return (
    <Card className={blocked ? "border-warning/60" : undefined}>
      <CardHeader className="pb-2">
        <CardTitle className="flex items-start justify-between gap-2 text-sm">
          <Link to="/p/$projectId" params={{ projectId: it.projectId }} search={{ tab: "tasks", task: t.id }} className="min-w-0 truncate hover:underline">
            {t.title}
          </Link>
          <span className="shrink-0 text-xs font-normal text-muted-foreground">{blocked ? "Blocked" : "PR in review"}</span>
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-2 text-xs text-muted-foreground">
        <div className="flex items-center gap-2">
          <span className="truncate">{it.projectName}</span>
          <span className="ml-auto">{ago(t.updatedAt)}</span>
        </div>
        {blocked && it.reason ? <div className="rounded-md bg-warning/10 p-2 text-foreground">{it.reason}</div> : null}
        {it.lastEvent?.text ? <p className="line-clamp-2">{it.lastEvent.text}</p> : null}
        <div className="flex flex-wrap items-center gap-2">
          {!blocked && t.pr?.number ? (
            <Button size="sm" asChild>
              <a href={t.pr.url} target="_blank" rel="noreferrer">
                <GitPullRequest /> Review PR #{t.pr.number} <ExternalLink />
              </a>
            </Button>
          ) : null}
          {blocked ? (
            <Button size="sm" variant={resuming ? "secondary" : "default"} aria-expanded={resuming} onClick={() => setResuming((v) => !v)}>
              Resume
            </Button>
          ) : null}
          <Button size="sm" variant="ghost" asChild>
            <Link to="/p/$projectId" params={{ projectId: it.projectId }} search={{ tab: "tasks", task: t.id }}>
              Open task
            </Link>
          </Button>
          {t.threadId ? (
            <Button size="sm" variant="ghost" asChild>
              <Link to="/t/$threadId" params={{ threadId: t.threadId }}>
                Session
              </Link>
            </Button>
          ) : null}
        </div>
        {blocked && resuming ? (
          <div className="text-foreground">
            <ResumeForm projectId={it.projectId} task={t} onDone={() => setResuming(false)} />
          </div>
        ) : null}
      </CardContent>
    </Card>
  );
}

/** A workflow gate that waits for a decision: approve it, send the run back, or add context, without leaving the inbox. */
export function GateItem({ it }: { it: InboxGate }) {
  return (
    <Card className="border-warning/60">
      <CardHeader className="pb-2">
        <CardTitle className="flex items-start justify-between gap-2 text-sm">
          <Link to="/p/$projectId" params={{ projectId: it.projectId }} search={{ tab: "workflows", run: it.runId }} className="min-w-0 truncate hover:underline">
            {it.workflowName}: {it.title}
          </Link>
          <span className="shrink-0 text-xs font-normal text-muted-foreground">Workflow gate</span>
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-2 text-xs text-muted-foreground">
        <div className="flex items-center gap-2">
          <span className="truncate">{it.projectName}</span>
          {it.since ? <span className="ml-auto">{ago(it.since)}</span> : null}
        </div>
        {it.summary ? <p className="line-clamp-4 whitespace-pre-wrap rounded-md bg-muted p-2 text-foreground">{it.summary}</p> : null}
        <div className="text-foreground">
          <GateActions runId={it.runId} nodeId={it.nodeId} compact />
        </div>
        <Button size="sm" variant="ghost" asChild>
          <Link to="/p/$projectId" params={{ projectId: it.projectId }} search={{ tab: "workflows", run: it.runId }}>
            Open the run
          </Link>
        </Button>
      </CardContent>
    </Card>
  );
}

export function InboxPage() {
  const q = useNeedsReview();
  if (q.isLoading) return <ListSkeleton rows={4} />;
  if (q.error || !q.data) return <ErrorState error={q.error} retry={() => void q.refetch()} />;
  const { tasks, threads, count } = q.data;
  const gates = q.data.gates ?? [];
  return (
    <ScrollArea className="h-full">
      <div className="mx-auto max-w-6xl space-y-8 p-4 md:p-8">
        <h1 className="flex items-center gap-2 text-xl font-semibold">
          <InboxIcon className="size-5" /> Needs review
          {count ? <span className="rounded-full bg-warning px-2 text-xs text-warning-foreground">{count}</span> : null}
        </h1>
        {!count ? <Empty title="Nothing waits for you">Blocked tasks, pull requests to review and sessions that ask for you show up here.</Empty> : null}
        {gates.length ? (
          <section aria-labelledby="inbox-gates" className="space-y-3">
            <h2 id="inbox-gates" className="text-sm font-medium text-muted-foreground">
              Workflow gates ({gates.length})
            </h2>
            <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
              {gates.map((it) => (
                <GateItem key={`${it.runId}/${it.nodeId}`} it={it} />
              ))}
            </div>
          </section>
        ) : null}
        {tasks.length ? (
          <section aria-labelledby="inbox-tasks" className="space-y-3">
            <h2 id="inbox-tasks" className="text-sm font-medium text-muted-foreground">
              Tasks ({tasks.length})
            </h2>
            <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
              {tasks.map((it) => (
                <TaskItem key={it.task.id} it={it} />
              ))}
            </div>
          </section>
        ) : null}
        {threads.length ? (
          <section aria-labelledby="inbox-threads" className="space-y-3">
            <h2 id="inbox-threads" className="text-sm font-medium text-muted-foreground">
              Sessions ({threads.length})
            </h2>
            <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
              {threads.map((it) => (
                <SessionCard key={it.thread.id} t={{ ...it.thread, projectName: it.projectName }} />
              ))}
            </div>
          </section>
        ) : null}
      </div>
    </ScrollArea>
  );
}
