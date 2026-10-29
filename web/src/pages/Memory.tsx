import { formatDistanceToNowStrict } from "date-fns";
import { Brain, Check, Pencil, Plus, Search, Trash2, X } from "lucide-react";
import { useMemo, useState } from "react";
import type { MemoryEntry } from "@/api/types";
import { useDecideMemory, useDeleteMemory, useMemory, useSaveMemory } from "@/api/queries";
import { Empty, ErrorState, ListSkeleton } from "@/components/app/common";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Textarea } from "@/components/ui/textarea";

function EntryDialog({ project, entry, open, onClose }: { project?: string; entry?: MemoryEntry; open: boolean; onClose: () => void }) {
  const save = useSaveMemory(project);
  const [f, setF] = useState({ title: entry?.title ?? "", content: entry?.content ?? "", tags: (entry?.tags ?? []).join(", ") });
  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <form
          onSubmit={(e) => {
            e.preventDefault();
            save.mutate(
              { id: entry?.id, input: { title: f.title.trim(), content: f.content, tags: f.tags.split(",").map((t) => t.trim()).filter(Boolean) } },
              { onSuccess: onClose },
            );
          }}
        >
          <DialogHeader>
            <DialogTitle>{entry ? "Edit memory" : "Add memory"}</DialogTitle>
            <DialogDescription>{project ? "Shared with every member of the project." : "Only you, in all your personal sessions."}</DialogDescription>
          </DialogHeader>
          <div className="my-4 space-y-4">
            <div className="space-y-2">
              <Label htmlFor="m-title">Title</Label>
              <Input id="m-title" value={f.title} onChange={(e) => setF({ ...f, title: e.target.value })} required autoFocus />
            </div>
            <div className="space-y-2">
              <Label htmlFor="m-content">Content</Label>
              <Textarea id="m-content" value={f.content} onChange={(e) => setF({ ...f, content: e.target.value })} rows={8} required />
            </div>
            <div className="space-y-2">
              <Label htmlFor="m-tags">Tags (comma separated)</Label>
              <Input id="m-tags" value={f.tags} onChange={(e) => setF({ ...f, tags: e.target.value })} />
            </div>
          </div>
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" disabled={save.isPending}>
              Save
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function Entry({ e, project, readOnly, onEdit, onDelete }: { e: MemoryEntry; project?: string; readOnly: boolean; onEdit: () => void; onDelete: () => void }) {
  const decide = useDecideMemory(project);
  const pending = e.status === "pending" || !!e.proposal;
  return (
    <li className={`rounded-lg border p-3 ${pending ? "border-warning/60 bg-warning/5" : ""}`}>
      <div className="flex items-start gap-2">
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <h3 className="text-sm font-medium">{e.title}</h3>
            {pending ? <Badge>{e.proposal ? "Proposed change" : "Suggested by the agent"}</Badge> : null}
            {(e.tags ?? []).map((t) => (
              <Badge key={t} variant="secondary" className="font-normal">
                {t}
              </Badge>
            ))}
          </div>
          <p className="mt-1 whitespace-pre-wrap break-words text-sm text-muted-foreground">{e.content}</p>
          {e.proposal ? (
            <div className="mt-2 rounded-md border bg-background p-2 text-sm">
              <div className="text-xs font-medium">Proposed: {e.proposal.title}</div>
              <p className="whitespace-pre-wrap break-words text-muted-foreground">{e.proposal.content}</p>
            </div>
          ) : null}
          {e.updated_at ? <div className="mt-1 text-xs text-muted-foreground">Updated {formatDistanceToNowStrict(new Date(e.updated_at))} ago{e.source ? ` · ${e.source}` : ""}</div> : null}
        </div>
        {!readOnly && !pending ? (
          <div className="flex shrink-0">
            <Button variant="ghost" size="icon" className="size-8" aria-label={`Edit ${e.title}`} onClick={onEdit}>
              <Pencil />
            </Button>
            <Button variant="ghost" size="icon" className="size-8" aria-label={`Delete ${e.title}`} onClick={onDelete}>
              <Trash2 />
            </Button>
          </div>
        ) : null}
      </div>
      {pending && !readOnly ? (
        <div className="mt-2 flex gap-2">
          <Button size="sm" onClick={() => decide.mutate({ id: e.id, action: "confirm" })} disabled={decide.isPending}>
            <Check /> Keep
          </Button>
          <Button size="sm" variant="outline" onClick={() => decide.mutate({ id: e.id, action: "reject" })} disabled={decide.isPending}>
            <X /> Discard
          </Button>
        </div>
      ) : null}
    </li>
  );
}

/** Browse, add, edit, delete and confirm memory entries of the user (no project) or a project. */
export function MemoryManager({ project, compact, readOnly = false }: { project?: string; compact?: boolean; readOnly?: boolean }) {
  const { data, isLoading, error, refetch } = useMemory(project);
  const del = useDeleteMemory(project);
  const [query, setQuery] = useState("");
  const [dialog, setDialog] = useState<{ entry?: MemoryEntry } | null>(null);
  const [confirm, setConfirm] = useState<MemoryEntry | null>(null);
  const list = useMemo(() => {
    const q = query.toLowerCase();
    return (data?.entries ?? [])
      .filter((e) => !q || `${e.title} ${e.content} ${(e.tags ?? []).join(" ")}`.toLowerCase().includes(q))
      .sort((a, b) => Number(b.status === "pending" || !!b.proposal) - Number(a.status === "pending" || !!a.proposal) || (b.updated_at ?? "").localeCompare(a.updated_at ?? ""));
  }, [data, query]);
  if (isLoading) return <ListSkeleton />;
  if (error) return <ErrorState error={error} retry={() => void refetch()} />;
  if (data && !data.enabled) return <Empty icon={<Brain className="size-8" />} title="Memory is not enabled on this server" />;
  const body = (
    <div className={compact ? "space-y-3 p-3" : "mx-auto max-w-3xl space-y-4 p-4 md:p-8"}>
      <div className="flex items-center gap-2">
        <div className="relative flex-1">
          <Search className="absolute left-2.5 top-2.5 size-4 text-muted-foreground" />
          <Input value={query} onChange={(e) => setQuery(e.target.value)} placeholder="Search memory" className="pl-8" aria-label="Search memory" />
        </div>
        {!readOnly ? (
          <Button onClick={() => setDialog({})}>
            <Plus /> Add
          </Button>
        ) : null}
      </div>
      {list.length ? (
        <ul className="space-y-2">
          {list.map((e) => (
            <Entry key={e.id} e={e} project={project} readOnly={readOnly} onEdit={() => setDialog({ entry: e })} onDelete={() => setConfirm(e)} />
          ))}
        </ul>
      ) : (
        <Empty icon={<Brain className="size-8" />} title={query ? "No matches" : "No memory yet"}>
          {query ? null : "The agent saves what it learns here; you can also add notes yourself."}
        </Empty>
      )}
      {dialog ? <EntryDialog key={dialog.entry?.id ?? "new"} project={project} entry={dialog.entry} open onClose={() => setDialog(null)} /> : null}
      <AlertDialog open={!!confirm} onOpenChange={(o) => !o && setConfirm(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete “{confirm?.title}”?</AlertDialogTitle>
            <AlertDialogDescription>The agent will no longer remember this.</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={() => confirm && del.mutate(confirm.id)}>Delete</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
  return compact ? body : <ScrollArea className="h-full">{body}</ScrollArea>;
}

export function MemoryPage() {
  return (
    <div className="flex h-full flex-col">
      <div className="mx-auto w-full max-w-3xl px-4 pt-4 md:px-8 md:pt-8">
        <h1 className="flex items-center gap-2 text-xl font-semibold">
          <Brain className="size-5" /> My memory
        </h1>
        <p className="text-sm text-muted-foreground">What the agent remembers about you across your personal sessions.</p>
      </div>
      <div className="min-h-0 flex-1">
        <MemoryManager />
      </div>
    </div>
  );
}
