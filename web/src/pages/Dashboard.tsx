import { useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { formatDistanceToNowStrict } from "date-fns";
import { Check, FolderPlus, LayoutDashboard, Plus, X } from "lucide-react";
import { useState } from "react";
import { api } from "@/api/client";
import { qk, toastError, useOverview } from "@/api/queries";
import type { Thread, ThreadStatus, ThreadSummary } from "@/api/types";
import { Empty, ErrorState, ListSkeleton, StatusBadge, UserAvatar, isActive, needsYou } from "@/components/app/common";
import { useDialogs } from "@/components/app/dialogs";
import { ImportEntry } from "@/components/app/github-import";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { ScrollArea } from "@/components/ui/scroll-area";

interface Row extends ThreadSummary {
  projectName: string;
}

const rank = (s: ThreadStatus) => (s === "waiting_approval" ? 0 : s === "waiting_input" ? 1 : s === "failed" ? 2 : s === "running" ? 3 : s === "queued" ? 4 : 5);
const ago = (iso: string) => `${formatDistanceToNowStrict(new Date(iso))} ago`;

export function SessionCard({ t }: { t: Row }) {
  const qc = useQueryClient();
  const [busy, setBusy] = useState(false);
  const decide = async (decision: "approve" | "deny") => {
    setBusy(true);
    try {
      const th = await api.get<Thread>(`/threads/${t.id}`);
      if (!th.pendingApproval) throw new Error("Nothing to approve any more.");
      await api.post(`/threads/${t.id}/resume`, { interruptId: th.pendingApproval.id, reason: "tool_approval", decision, interactive: true });
      void qc.invalidateQueries({ queryKey: qk.overview });
    } catch (e) {
      toastError(e);
    } finally {
      setBusy(false);
    }
  };
  return (
    <Card className={needsYou(t.status) ? "border-warning/60" : undefined}>
      <CardHeader className="pb-2">
        <CardTitle className="flex items-start justify-between gap-2 text-sm">
          <Link to="/t/$threadId" params={{ threadId: t.id }} className="min-w-0 truncate hover:underline">
            {t.title || "(untitled)"}
          </Link>
          <StatusBadge status={t.status} />
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-2 text-xs text-muted-foreground">
        <div className="flex items-center gap-2">
          <span className="truncate">{t.projectName}</span>
          {t.createdBy ? (
            <span className="ml-auto flex items-center gap-1">
              <UserAvatar login={t.createdBy} className="size-4 text-[8px]" /> {t.createdBy}
            </span>
          ) : null}
        </div>
        {t.status === "waiting_approval" ? (
          <div className="space-y-2 rounded-md bg-warning/10 p-2 text-foreground">
            <div>
              Wants to run <code>{t.pendingTool || "a tool"}</code>
            </div>
            <div className="flex gap-2">
              <Button size="sm" disabled={busy} onClick={() => void decide("approve")}>
                <Check /> Approve
              </Button>
              <Button size="sm" variant="destructive" disabled={busy} onClick={() => void decide("deny")}>
                <X /> Deny
              </Button>
              <Button size="sm" variant="ghost" asChild>
                <Link to="/t/$threadId" params={{ threadId: t.id }}>
                  Review
                </Link>
              </Button>
            </div>
          </div>
        ) : null}
        {t.status === "waiting_input" ? <div className="rounded-md bg-warning/10 p-2 text-foreground">{t.pendingQuestion || "The agent has a question."}</div> : null}
        <div>Last activity {ago(t.updatedAt)}</div>
      </CardContent>
    </Card>
  );
}

export function Dashboard() {
  const { data, isLoading, error, refetch } = useOverview();
  const { newProject, newSession, importGithub } = useDialogs();
  if (isLoading) return <ListSkeleton rows={5} />;
  if (error || !data) return <ErrorState error={error} retry={() => void refetch()} />;
  const rows: Row[] = [
    ...data.personal.map((t) => ({ ...t, projectName: "Personal" })),
    ...data.projects.flatMap((p) => p.threads.map((t) => ({ ...t, projectName: p.name }))),
  ].filter((t) => t.kind !== "supervisor");
  const active = rows.filter((t) => isActive(t.status)).sort((a, b) => rank(a.status) - rank(b.status) || b.updatedAt.localeCompare(a.updatedAt));
  const recent = rows.filter((t) => !isActive(t.status)).sort((a, b) => b.updatedAt.localeCompare(a.updatedAt)).slice(0, 12);
  return (
    <ScrollArea className="h-full">
      <div className="mx-auto max-w-6xl space-y-8 p-4 md:p-8">
        <div className="flex items-center justify-between gap-2">
          <h1 className="flex items-center gap-2 text-xl font-semibold">
            <LayoutDashboard className="size-5" /> Dashboard
          </h1>
          <Button onClick={() => newSession()}>
            <Plus /> New session
          </Button>
        </div>
        {!data.projects.length && !data.personal.length ? (
          <Empty icon={<FolderPlus className="size-10" />} title="Create your first project">
            <p>A project is a repository whose sessions, memory and members are shared with your team.</p>
            <div className="mt-3 flex flex-wrap justify-center gap-2">
              <Button onClick={newProject}>
                <Plus /> New project
              </Button>
              <ImportEntry onImport={importGithub} />
            </div>
          </Empty>
        ) : null}
        <section aria-labelledby="active-h" className="space-y-3">
          <h2 id="active-h" className="text-sm font-medium text-muted-foreground">
            Running and waiting ({active.length})
          </h2>
          {active.length ? (
            <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
              {active.map((t) => (
                <SessionCard key={t.id} t={t} />
              ))}
            </div>
          ) : (
            <p className="text-sm text-muted-foreground">Nothing is running. Start a session from the sidebar.</p>
          )}
        </section>
        {recent.length ? (
          <section aria-labelledby="recent-h" className="space-y-3">
            <h2 id="recent-h" className="text-sm font-medium text-muted-foreground">
              Recent sessions
            </h2>
            <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
              {recent.map((t) => (
                <SessionCard key={t.id} t={t} />
              ))}
            </div>
          </section>
        ) : null}
      </div>
    </ScrollArea>
  );
}
