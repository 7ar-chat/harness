import { Link } from "@tanstack/react-router";
import { formatDistanceToNowStrict } from "date-fns";
import { Brain, Check, ChevronDown, FileText, History, Info, Pencil, Pin, PinOff, Plus, Search, Sparkles, Trash2, X } from "lucide-react";
import { useMemo, useState, type ReactNode } from "react";
import type { MemoryEntry } from "@/api/types";
import { useBulkDecide, useDecideMemory, useDeleteMemory, useMemory, useMemoryStats, useSaveMemory, type MemScope } from "@/api/queries";
import { Empty, ErrorState, ListSkeleton } from "@/components/app/common";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { cn } from "@/lib/utils";

// ---- entry helpers ----
export const kindOf = (e: MemoryEntry): string => /^memories\/semantic\/([^/]+)\//.exec(e.path ?? "")?.[1] ?? (e.path?.startsWith("memories/core/") ? "core" : "notes");
export const isPending = (e: MemoryEntry) => e.status === "pending" || !!e.proposal;
const isPinned = (e: MemoryEntry) => e.metadata?.pinned === true;
const isRepo = (e: MemoryEntry) => e.metadata?.source === "repo";
const isAuto = (e: MemoryEntry) => e.metadata?.auto === true;
const ago = (iso?: string | null) => (iso ? `${formatDistanceToNowStrict(new Date(iso))} ago` : "");
const threadOf = (e: MemoryEntry) => e.sourceThreadId || e.metadata?.history?.find((h) => h.thread)?.thread;

const LAYERS = {
  shared: "Project memory is shared with every member of the project and given to agents working on it.",
  mine: "Your memory in this project is private to you and used when you are the one asking.",
  user: "Your global memory follows you into all your personal sessions.",
};

function ImportanceDot({ v }: { v?: number | null }) {
  if (v === null || v === undefined) return null;
  const level = v >= 0.7 ? "high" : v >= 0.4 ? "medium" : "low";
  return <span role="img" aria-label={`Importance ${level}`} title={`Importance ${Math.round(v * 100)}%`} className={cn("inline-block size-2 rounded-full", level === "high" ? "bg-warning" : level === "medium" ? "bg-muted-foreground" : "bg-muted-foreground/30")} />;
}

// ---- add / edit dialog with revision history ----
function EntryDialog({ scope, entry, open, onClose }: { scope: MemScope; entry?: MemoryEntry; open: boolean; onClose: () => void }) {
  const save = useSaveMemory(scope);
  const [f, setF] = useState({ title: entry?.title ?? "", content: entry?.content ?? "", tags: (entry?.tags ?? []).join(", "), pinned: entry ? isPinned(entry) : false });
  const history = entry?.metadata?.history ?? [];
  const tags = () => f.tags.split(",").map((t) => t.trim()).filter(Boolean);
  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[90vh] overflow-y-auto">
        <form
          onSubmit={(e) => {
            e.preventDefault();
            const input = { title: f.title.trim(), content: f.content, tags: tags(), pinned: f.pinned };
            save.mutate({ id: entry?.id, input }, { onSuccess: onClose });
          }}
        >
          <DialogHeader>
            <DialogTitle>{entry ? "Edit memory" : "Add memory"}</DialogTitle>
            <DialogDescription>{scope.project ? (scope.mine ? LAYERS.mine : LAYERS.shared) : LAYERS.user}</DialogDescription>
          </DialogHeader>
          {entry && isRepo(entry) ? (
            <p className="mt-3 rounded-md border border-warning/50 bg-warning/10 p-2 text-xs">This entry comes from the repository{entry.metadata?.file ? ` (${entry.metadata.file})` : ""}. The next import overwrites your edits.</p>
          ) : null}
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
            <div className="flex items-center gap-3">
              <Switch id="m-pin" checked={f.pinned} onCheckedChange={(pinned) => setF({ ...f, pinned })} />
              <Label htmlFor="m-pin" className="font-normal">
                Pin: always give this to the agent in every run
              </Label>
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
        {history.length ? (
          <section aria-labelledby="hist-h" className="mt-2 border-t pt-4">
            <h3 id="hist-h" className="mb-2 flex items-center gap-1.5 text-sm font-medium">
              <History className="size-4" /> Earlier versions
            </h3>
            <ol className="space-y-3 border-l pl-4">
              {history.map((h, i) => (
                <li key={i} className="relative text-sm">
                  <span className="absolute -left-[1.3rem] top-1.5 size-2 rounded-full bg-muted-foreground/50" />
                  <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
                    {h.replaced_at ? <time dateTime={h.replaced_at}>Replaced {ago(h.replaced_at)}</time> : null}
                    {h.thread ? (
                      <Link to="/t/$threadId" params={{ threadId: h.thread }} className="underline" onClick={onClose}>
                        session
                      </Link>
                    ) : null}
                    <Button type="button" variant="outline" size="sm" className="ml-auto h-6 text-xs" disabled={save.isPending} onClick={() => entry && save.mutate({ id: entry.id, input: { content: h.content, ...(h.title ? { title: h.title } : {}) } }, { onSuccess: onClose })}>
                      Restore this version
                    </Button>
                  </div>
                  <p className="mt-1 line-clamp-4 whitespace-pre-wrap break-words text-muted-foreground">{h.content}</p>
                </li>
              ))}
            </ol>
          </section>
        ) : null}
      </DialogContent>
    </Dialog>
  );
}

// ---- cards ----
function Tag({ children }: { children: ReactNode }) {
  return (
    <Badge variant="secondary" className="font-normal">
      {children}
    </Badge>
  );
}

function ReviewCard({ e, scope, readOnly }: { e: MemoryEntry; scope: MemScope; readOnly: boolean }) {
  const decide = useDecideMemory(scope);
  const thread = threadOf(e);
  return (
    <li className="rounded-lg border border-warning/60 bg-warning/5 p-3" aria-label={`Review: ${e.proposal?.title || e.title}`}>
      <div className="flex flex-wrap items-center gap-2">
        <h3 className="text-sm font-medium">{e.proposal?.title || e.title}</h3>
        <Badge variant="outline">{kindOf(e)}</Badge>
        <Badge>{e.proposal ? "Suggested change" : "New"}</Badge>
        {isAuto(e) ? (
          <Badge variant="secondary" className="gap-1">
            <Sparkles className="size-3" /> auto
          </Badge>
        ) : null}
      </div>
      {e.metadata?.reason ? <p className="mt-1 text-xs italic text-muted-foreground">Why: {e.metadata.reason}</p> : null}
      {e.proposal ? (
        <div className="mt-2 grid gap-2 text-sm sm:grid-cols-2">
          <div className="rounded-md border bg-background p-2">
            <div className="mb-1 text-xs font-medium text-muted-foreground">Now</div>
            <p className="whitespace-pre-wrap break-words">{e.content}</p>
          </div>
          <div className="rounded-md border border-warning/40 bg-background p-2">
            <div className="mb-1 text-xs font-medium text-muted-foreground">Proposed</div>
            <p className="whitespace-pre-wrap break-words">{e.proposal.content}</p>
          </div>
        </div>
      ) : (
        <p className="mt-2 whitespace-pre-wrap break-words text-sm">{e.content}</p>
      )}
      <div className="mt-3 flex flex-wrap items-center gap-2">
        {!readOnly ? (
          <>
            <Button size="sm" onClick={() => decide.mutate({ id: e.id, action: "confirm" })} disabled={decide.isPending}>
              <Check /> Keep
            </Button>
            <Button size="sm" variant="outline" onClick={() => decide.mutate({ id: e.id, action: "reject" })} disabled={decide.isPending}>
              <X /> Discard
            </Button>
          </>
        ) : null}
        {thread ? (
          <Link to="/t/$threadId" params={{ threadId: thread }} className="ml-auto text-xs text-muted-foreground underline">
            From a session
          </Link>
        ) : null}
        <span className="text-xs text-muted-foreground">{ago(e.updated_at)}</span>
      </div>
    </li>
  );
}

function EntryCard({ e, scope, readOnly, onEdit }: { e: MemoryEntry; scope: MemScope; readOnly: boolean; onEdit: () => void }) {
  const save = useSaveMemory(scope);
  const del = useDeleteMemory(scope);
  const [open, setOpen] = useState(false);
  const [confirm, setConfirm] = useState(false);
  const pinned = isPinned(e);
  const long = e.content.length > 240 || e.content.split("\n").length > 4;
  const hist = e.metadata?.history?.length ?? 0;
  return (
    <li className={cn("rounded-lg border p-3", pinned && "border-primary/40 bg-primary/5", isRepo(e) && "border-dashed")}>
      <div className="flex items-start gap-2">
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <ImportanceDot v={e.importance} />
            <h3 className="text-sm font-medium">{e.title}</h3>
            <Badge variant="outline">{kindOf(e)}</Badge>
            {isAuto(e) ? (
              <Badge variant="secondary" className="gap-1">
                <Sparkles className="size-3" /> auto
              </Badge>
            ) : null}
            {isRepo(e) ? (
              <Badge variant="secondary" className="gap-1">
                <FileText className="size-3" /> from repository
              </Badge>
            ) : null}
            {(e.tags ?? []).map((t) => (
              <Tag key={t}>{t}</Tag>
            ))}
          </div>
          <p className={cn("mt-1 whitespace-pre-wrap break-words text-sm text-muted-foreground", !open && "line-clamp-4")}>{e.content}</p>
          {long ? (
            <button type="button" className="mt-1 inline-flex items-center gap-1 text-xs text-primary" onClick={() => setOpen(!open)} aria-expanded={open}>
              <ChevronDown className={cn("size-3 transition-transform", open && "rotate-180")} /> {open ? "Show less" : "Show more"}
            </button>
          ) : null}
          <div className="mt-1 text-xs text-muted-foreground">
            {ago(e.updated_at) ? `Updated ${ago(e.updated_at)}` : ""}
            {hist ? ` · ${hist} earlier version${hist > 1 ? "s" : ""}` : ""}
          </div>
        </div>
        {!readOnly ? (
          <div className="flex shrink-0">
            <Button variant="ghost" size="icon" className="size-8" aria-pressed={pinned} aria-label={pinned ? `Unpin ${e.title}` : `Pin ${e.title}`} title={pinned ? "Pinned: always given to the agent" : "Pin: always give to the agent"} onClick={() => save.mutate({ id: e.id, input: { pinned: !pinned } })}>
              {pinned ? <PinOff /> : <Pin />}
            </Button>
            {hist ? (
              <Button variant="ghost" size="icon" className="size-8" aria-label={`History of ${e.title}`} title="Revision history" onClick={onEdit}>
                <History />
              </Button>
            ) : null}
            <Button variant="ghost" size="icon" className="size-8" aria-label={`Edit ${e.title}`} onClick={onEdit}>
              <Pencil />
            </Button>
            <Button variant="ghost" size="icon" className="size-8" aria-label={`Delete ${e.title}`} onClick={() => setConfirm(true)}>
              <Trash2 />
            </Button>
          </div>
        ) : pinned ? (
          <Pin className="size-4 text-primary" aria-label="Pinned" />
        ) : null}
      </div>
      <AlertDialog open={confirm} onOpenChange={setConfirm}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete “{e.title}”?</AlertDialogTitle>
            <AlertDialogDescription>The agent will no longer remember this.</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={() => del.mutate(e.id)}>Delete</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </li>
  );
}

// ---- stats ----
const n = (v?: number) => v ?? 0;
function StatsStrip({ scope }: { scope: MemScope }) {
  const { data: s } = useMemoryStats(scope);
  if (!s) return null;
  const kinds = Object.entries(s.byKind ?? {}).filter(([, c]) => c > 0);
  const total = kinds.reduce((a, [, c]) => a + c, 0);
  const sv = s.service ?? {};
  return (
    <div className="flex flex-wrap items-center gap-x-5 gap-y-2 rounded-lg border bg-card p-3 text-sm">
      <span>
        <b>{s.active}</b> active
      </span>
      <span>
        <b>{s.pending}</b> to review
      </span>
      <span>
        <b>{s.pinned}</b> pinned
      </span>
      {total ? (
        <div className="min-w-32 flex-1" aria-label={`By kind: ${kinds.map(([k, c]) => `${k} ${c}`).join(", ")}`}>
          <div className="flex h-2 overflow-hidden rounded-full bg-muted" role="img" aria-hidden="true">
            {kinds.map(([k, c], i) => (
              <div key={k} title={`${k}: ${c}`} className="h-full" style={{ width: `${(c / total) * 100}%`, background: `color-mix(in oklab, var(--primary) ${Math.max(25, 100 - i * 18)}%, transparent)` }} />
            ))}
          </div>
        </div>
      ) : null}
      <Popover>
        <PopoverTrigger asChild>
          <Button variant="ghost" size="sm" className="ml-auto">
            <Info /> How agents use it
          </Button>
        </PopoverTrigger>
        <PopoverContent className="w-80 space-y-2 text-sm" align="end">
          <p>
            Recalled into <b>{n(sv.recallRuns)}</b> runs ({n(sv.recallItems)} items, {n(sv.recallPinned)} pinned).
          </p>
          <p>
            Searched {n(sv.searches)} times, {n(sv.searchHits)} hits.
          </p>
          <p>
            Proposed <b>{n(sv.extractProposed)}</b> memories after runs, skipped {n(sv.extractSkipped)} duplicates{n(sv.extractErrors) ? `, ${n(sv.extractErrors)} errors` : ""}.
          </p>
          <p>
            Embeddings: {s.embeddings?.enabled ? `on${s.embeddings.model ? ` (${s.embeddings.model})` : ""}, ${n(s.embeddings.vectors)} vectors` : "off"}.
          </p>
          <p className="text-xs text-muted-foreground">Counters are since the server started.</p>
        </PopoverContent>
      </Popover>
      {s.embeddings && !s.embeddings.enabled ? <p className="basis-full text-xs text-muted-foreground">Semantic search is off: agents find memories by keywords only.</p> : null}
    </div>
  );
}

// ---- the workspace ----
type Sort = "recent" | "importance";

function Layer({ scope, readOnly, compact }: { scope: MemScope; readOnly: boolean; compact?: boolean }) {
  const { data, isLoading, error, refetch } = useMemory(scope);
  const bulk = useBulkDecide(scope);
  const [query, setQuery] = useState("");
  const [kind, setKind] = useState<string | null>(null);
  const [sort, setSort] = useState<Sort>("recent");
  const [dialog, setDialog] = useState<{ entry?: MemoryEntry } | null>(null);
  const [bulkAsk, setBulkAsk] = useState<"confirm" | "reject" | null>(null);

  const all = data?.entries ?? [];
  const filtered = useMemo(() => {
    const q = query.toLowerCase();
    const cmp = (a: MemoryEntry, b: MemoryEntry) => (sort === "importance" ? (b.importance ?? -1) - (a.importance ?? -1) : 0) || (b.updated_at ?? "").localeCompare(a.updated_at ?? "");
    return all
      .filter((e) => (!q || `${e.title} ${e.content} ${(e.tags ?? []).join(" ")}`.toLowerCase().includes(q)) && (!kind || kindOf(e) === kind))
      .sort(cmp);
  }, [all, query, kind, sort]);
  const review = filtered.filter(isPending);
  const active = filtered.filter((e) => !isPending(e));
  const pinned = active.filter(isPinned);
  const groups = useMemo(() => {
    const m = new Map<string, MemoryEntry[]>();
    for (const e of active.filter((x) => !isPinned(x))) m.set(kindOf(e), [...(m.get(kindOf(e)) ?? []), e]);
    return [...m.entries()].sort((a, b) => b[1].length - a[1].length);
  }, [active]);
  const kinds = useMemo(() => [...new Set(all.map(kindOf))].sort(), [all]);

  if (isLoading) return <ListSkeleton />;
  if (error) return <ErrorState error={error} retry={() => void refetch()} />;
  if (data && !data.enabled) return <Empty icon={<Brain className="size-8" />} title="Memory is not enabled on this server" />;
  const layerHint = scope.project ? (scope.mine ? LAYERS.mine : LAYERS.shared) : LAYERS.user;
  return (
    <div className="space-y-5">
      <StatsStrip scope={scope} />
      <div className="flex flex-wrap items-center gap-2">
        <div className="relative min-w-40 flex-1">
          <Search className="absolute left-2.5 top-2.5 size-4 text-muted-foreground" />
          <Input value={query} onChange={(e) => setQuery(e.target.value)} placeholder="Search title, content, tags" className="pl-8" aria-label="Search memory" />
        </div>
        <Select value={sort} onValueChange={(v) => setSort(v as Sort)}>
          <SelectTrigger className="w-36" aria-label="Sort">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="recent">Most recent</SelectItem>
            <SelectItem value="importance">Importance</SelectItem>
          </SelectContent>
        </Select>
        {!readOnly ? (
          <Button onClick={() => setDialog({})}>
            <Plus /> Add
          </Button>
        ) : null}
      </div>
      {kinds.length > 1 ? (
        <div className="flex flex-wrap gap-1.5" role="group" aria-label="Filter by kind">
          {kinds.map((k) => (
            <Button key={k} size="sm" variant={kind === k ? "default" : "outline"} className="h-7 rounded-full text-xs" aria-pressed={kind === k} onClick={() => setKind(kind === k ? null : k)}>
              {k}
            </Button>
          ))}
        </div>
      ) : null}

      {review.length ? (
        <section aria-labelledby="rev-h" className="space-y-2">
          <div className="flex flex-wrap items-center gap-2">
            <h2 id="rev-h" className="text-sm font-semibold">
              Needs review ({review.length})
            </h2>
            {!readOnly && review.length > 1 ? (
              <div className="ml-auto flex gap-2">
                <Button size="sm" variant="outline" onClick={() => setBulkAsk("confirm")}>
                  <Check /> Keep all
                </Button>
                <Button size="sm" variant="ghost" onClick={() => setBulkAsk("reject")}>
                  Discard all
                </Button>
              </div>
            ) : null}
          </div>
          <ul className="space-y-2">
            {review.map((e) => (
              <ReviewCard key={e.id} e={e} scope={scope} readOnly={readOnly} />
            ))}
          </ul>
        </section>
      ) : null}

      {pinned.length ? (
        <section aria-labelledby="pin-h" className="space-y-2">
          <h2 id="pin-h" className="flex items-center gap-1.5 text-sm font-semibold">
            <Pin className="size-4" /> Pinned ({pinned.length})
            <span className="text-xs font-normal text-muted-foreground">always given to the agent</span>
          </h2>
          <ul className="space-y-2">
            {pinned.map((e) => (
              <EntryCard key={e.id} e={e} scope={scope} readOnly={readOnly} onEdit={() => setDialog({ entry: e })} />
            ))}
          </ul>
        </section>
      ) : null}

      {groups.map(([k, list]) => (
        <section key={k} aria-label={`${k} memories`} className="space-y-2">
          <h2 className="text-sm font-semibold capitalize">
            {k} <span className="font-normal text-muted-foreground">({list.length})</span>
          </h2>
          <ul className="space-y-2">
            {list.map((e) => (
              <EntryCard key={e.id} e={e} scope={scope} readOnly={readOnly} onEdit={() => setDialog({ entry: e })} />
            ))}
          </ul>
        </section>
      ))}

      {!review.length && !pinned.length && !groups.length ? (
        <Empty icon={<Brain className="size-8" />} title={query || kind ? "No matches" : "No memory yet"}>
          {query || kind ? null : (
            <div className="space-y-2 text-left">
              <p>{layerHint}</p>
              <p className="text-xs">The agent proposes what is worth keeping after each run; you review it here. You can also add notes yourself and pin the ones every run needs.</p>
            </div>
          )}
        </Empty>
      ) : null}

      {dialog ? <EntryDialog key={dialog.entry?.id ?? "new"} scope={scope} entry={dialog.entry} open onClose={() => setDialog(null)} /> : null}
      <AlertDialog open={!!bulkAsk} onOpenChange={(o) => !o && setBulkAsk(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{bulkAsk === "confirm" ? `Keep all ${review.length} suggestions?` : `Discard all ${review.length} suggestions?`}</AlertDialogTitle>
            <AlertDialogDescription>{bulkAsk === "confirm" ? "They become active memories the agent can use." : "They are dropped and will not be suggested again."}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={() => bulkAsk && bulk.mutate({ ids: review.map((e) => e.id), action: bulkAsk })}>{bulkAsk === "confirm" ? "Keep all" : "Discard all"}</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
      {compact ? null : <div className="h-4" />}
    </div>
  );
}

/** The memory of a project (shared and private layers) or the user's global memory. */
export function MemoryManager({ project, compact, readOnly = false }: { project?: string; compact?: boolean; readOnly?: boolean }) {
  const [layer, setLayer] = useState<"shared" | "mine">("shared");
  const body = (
    <div className={compact ? "space-y-4 p-3" : "mx-auto max-w-3xl space-y-4 p-4 md:p-8"}>
      {project ? (
        <Tabs value={layer} onValueChange={(v) => setLayer(v === "mine" ? "mine" : "shared")}>
          <TabsList>
            <TabsTrigger value="shared">Project</TabsTrigger>
            <TabsTrigger value="mine">Me in this project</TabsTrigger>
          </TabsList>
        </Tabs>
      ) : null}
      <Layer key={`${project}-${layer}`} scope={{ project, mine: !!project && layer === "mine" }} readOnly={readOnly} compact={compact} />
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
        <p className="text-sm text-muted-foreground">Global: what the agent remembers about you across your personal sessions. Project memories live in each project.</p>
      </div>
      <div className="min-h-0 flex-1">
        <MemoryManager />
      </div>
    </div>
  );
}
