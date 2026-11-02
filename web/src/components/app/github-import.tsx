import { Link, useNavigate } from "@tanstack/react-router";
import { ExternalLink, Lock, Search } from "lucide-react";
import { useEffect, useState } from "react";
import { ApiError } from "@/api/client";
import { toastError, useConnectors, useGithubRepos, useImportProject } from "@/api/queries";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { toast } from "sonner";
import { Empty, ListSkeleton } from "./common";

export const GithubMark = ({ className }: { className?: string }) => (
  <svg viewBox="0 0 16 16" className={className} fill="currentColor" aria-hidden="true">
    <path d="M8 0C3.58 0 0 3.58 0 8a8 8 0 0 0 5.47 7.59c.4.07.55-.17.55-.38v-1.33c-2.23.48-2.7-1.07-2.7-1.07-.36-.93-.89-1.17-.89-1.17-.73-.5.05-.49.05-.49.8.06 1.23.83 1.23.83.72 1.22 1.88.87 2.33.66.07-.52.28-.87.5-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82a7.6 7.6 0 0 1 4 0c1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.28.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48v2.2c0 .21.15.46.55.38A8 8 0 0 0 16 8c0-4.42-3.58-8-8-8Z" />
  </svg>
);

/** True when GitHub is configured and the user's account is connected. */
export function useGithubConnector() {
  const c = useConnectors().data?.find((x) => x.kind === "github");
  return { connector: c, ready: !!c?.connected };
}

function useDebounced<T>(v: T, ms = 300): T {
  const [d, setD] = useState(v);
  useEffect(() => {
    const t = setTimeout(() => setD(v), ms);
    return () => clearTimeout(t);
  }, [v, ms]);
  return d;
}

export function ImportGithubDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const [q, setQ] = useState("");
  const dq = useDebounced(q);
  const repos = useGithubRepos(dq, open);
  const imp = useImportProject();
  const navigate = useNavigate();
  const doImport = (fullName: string) =>
    imp.mutate(fullName, {
      onSuccess: (p) => {
        onClose();
        void navigate({ to: "/p/$projectId", params: { projectId: p.id }, search: { tab: undefined } });
      },
      onError: (e) => (e instanceof ApiError && e.code === "already_imported" ? toast.info("That repository is already a project.") : toastError(e)),
    });
  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-w-xl">
        <DialogHeader>
          <DialogTitle>Import a project from GitHub</DialogTitle>
          <DialogDescription>Pick a repository the GitHub App can access.</DialogDescription>
        </DialogHeader>
        <div className="relative">
          <Search className="absolute left-2.5 top-2.5 size-4 text-muted-foreground" />
          <Input value={q} onChange={(e) => setQ(e.target.value)} placeholder="Search repositories" className="pl-8" aria-label="Search repositories" autoFocus />
        </div>
        <div className="max-h-80 min-h-32 overflow-y-auto rounded-md border">
          {repos.isLoading ? (
            <ListSkeleton rows={3} />
          ) : repos.error ? (
            <Empty title="Could not list repositories">{repos.error instanceof Error ? repos.error.message : ""}</Empty>
          ) : repos.data?.repos.length ? (
            <ul className="divide-y">
              {repos.data.repos.map((r) => (
                <li key={r.fullName} className="flex items-center gap-3 p-3">
                  <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-1.5 text-sm font-medium">
                      {r.private ? <Lock className="size-3.5 text-muted-foreground" aria-label="Private" /> : null}
                      <span className="truncate">{r.fullName}</span>
                    </div>
                    {r.description ? <p className="truncate text-xs text-muted-foreground">{r.description}</p> : null}
                  </div>
                  {r.imported ? (
                    <Badge variant="secondary" asChild>
                      <Link to="/p/$projectId" params={{ projectId: r.imported }} search={{ tab: undefined }} onClick={onClose}>
                        Imported
                      </Link>
                    </Badge>
                  ) : (
                    <Button size="sm" disabled={imp.isPending} onClick={() => doImport(r.fullName)}>
                      Import
                    </Button>
                  )}
                </li>
              ))}
            </ul>
          ) : (
            <Empty title="No repositories found" />
          )}
        </div>
        {repos.data?.installUrl ? (
          <a href={repos.data.installUrl} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 text-sm text-primary underline underline-offset-2">
            <ExternalLink className="size-3.5" /> Install the GitHub App on more repositories
          </a>
        ) : null}
      </DialogContent>
    </Dialog>
  );
}

/** "Import from GitHub": opens the picker when connected, else points to the profile's connectors. */
export function ImportEntry({ onImport, onNavigate, variant = "outline" }: { onImport: () => void; onNavigate?: () => void; variant?: "outline" | "default" }) {
  const { connector, ready } = useGithubConnector();
  if (!connector?.configured) return null;
  return ready ? (
    <Button type="button" variant={variant} onClick={onImport}>
      <GithubMark className="size-4" /> Import from GitHub
    </Button>
  ) : (
    <Button type="button" variant={variant} asChild>
      <Link to="/profile" onClick={onNavigate}>
        <GithubMark className="size-4" /> Connect GitHub to import
      </Link>
    </Button>
  );
}
