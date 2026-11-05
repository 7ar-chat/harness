import { formatDistanceToNowStrict } from "date-fns";
import { ArrowDown, ArrowUp, Copy, Download, FileText, Search, Trash2, Upload } from "lucide-react";
import { useMemo, useRef, useState } from "react";
import { toast } from "sonner";
import { downloadText, isUnsupported } from "@/api/client";
import { toastError, useDeleteDocument, useDocuments, useUploadDocument, type UserDocument } from "@/api/queries";
import { ErrorState, ListSkeleton, Spinner } from "@/components/app/common";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { cn } from "@/lib/utils";

/** Mirrors features/documents: what the server accepts. */
const TYPES: Record<string, string> = {
  pdf: "PDF", doc: "Word", docx: "Word", odt: "Text", rtf: "Text", txt: "Text", md: "Markdown", csv: "Spreadsheet", json: "JSON",
  xlsx: "Spreadsheet", pptx: "Slides", png: "Image", jpg: "Image", jpeg: "Image", webp: "Image",
};
const ACCEPT = Object.keys(TYPES).map((e) => "." + e).join(",");
const MAX_FILE = 20 << 20;
const MAX_FILES = 50;
const MAX_TOTAL = 200 << 20;
const WORKSPACE = "/workspace/documents/";

const ext = (name: string) => name.slice(name.lastIndexOf(".") + 1).toLowerCase();
const kind = (name: string) => TYPES[ext(name)] ?? "File";
const size = (n: number) => (n >= 1 << 20 ? `${(n / (1 << 20)).toFixed(1)} MB` : n >= 1 << 10 ? `${Math.round(n / (1 << 10))} KB` : `${n} B`);
const ago = (iso: string) => `${formatDistanceToNowStrict(new Date(iso))} ago`;

type SortKey = "name" | "size" | "modified" | "type";

/** The user's reference files: a flat, sortable list every session and agent of theirs can use (a CV, a cover-letter template...). */
export function ReferencesPanel() {
  const docs = useDocuments();
  const upload = useUploadDocument();
  const remove = useDeleteDocument();
  const input = useRef<HTMLInputElement>(null);
  const [q, setQ] = useState("");
  const [sort, setSort] = useState<SortKey>("modified");
  const [desc, setDesc] = useState(true);
  const [picked, setPicked] = useState<Set<string>>(new Set());
  const [confirm, setConfirm] = useState<string[]>();
  const [drag, setDrag] = useState(false);

  const all = docs.data ?? [];
  const total = all.reduce((n, d) => n + d.size, 0);
  const shown = useMemo(() => {
    const s = q.trim().toLowerCase();
    const cmp: Record<SortKey, (a: UserDocument, b: UserDocument) => number> = {
      name: (a, b) => a.name.localeCompare(b.name, undefined, { numeric: true }),
      size: (a, b) => a.size - b.size,
      modified: (a, b) => a.modified.localeCompare(b.modified),
      type: (a, b) => kind(a.name).localeCompare(kind(b.name)) || a.name.localeCompare(b.name),
    };
    return all.filter((d) => !s || d.name.toLowerCase().includes(s) || kind(d.name).toLowerCase().includes(s)).sort((a, b) => (desc ? -1 : 1) * cmp[sort](a, b));
  }, [all, q, sort, desc]);

  const add = (files: File[]) => {
    for (const f of files) {
      if (!(ext(f.name) in TYPES)) toast.error(`${f.name}: not a supported type (PDF, Word, text, spreadsheet, slides or image).`);
      else if (f.size > MAX_FILE) toast.error(`${f.name}: larger than ${MAX_FILE >> 20} MB.`);
      else if (f.size === 0) toast.error(`${f.name}: the file is empty.`);
      else upload.mutate(f);
    }
  };
  const toggle = (name: string) =>
    setPicked((p) => {
      const n = new Set(p);
      if (!n.delete(name)) n.add(name);
      return n;
    });
  const allPicked = shown.length > 0 && shown.every((d) => picked.has(d.name));
  const head = (key: SortKey, label: string, className?: string) => (
    <button type="button" className={cn("inline-flex items-center gap-1 hover:text-foreground", className)} onClick={() => (sort === key ? setDesc(!desc) : (setSort(key), setDesc(key !== "name" && key !== "type")))}>
      {label}
      {sort === key ? desc ? <ArrowDown className="size-3" /> : <ArrowUp className="size-3" /> : null}
    </button>
  );

  if (docs.error && isUnsupported(docs.error)) return <p className="text-sm text-muted-foreground">This server keeps no reference library.</p>;
  if (docs.error) return <ErrorState error={docs.error} retry={() => void docs.refetch()} />;

  return (
    <div
      className="relative space-y-3"
      onDragOver={(e) => {
        if (e.dataTransfer.types.includes("Files")) {
          e.preventDefault();
          setDrag(true);
        }
      }}
      onDragLeave={(e) => e.currentTarget.contains(e.relatedTarget as Node) || setDrag(false)}
      onDrop={(e) => {
        e.preventDefault();
        setDrag(false);
        add(Array.from(e.dataTransfer.files));
      }}
    >
      <input ref={input} type="file" multiple accept={ACCEPT} className="sr-only" aria-label="Choose references to add" onChange={(e) => (add(Array.from(e.target.files ?? [])), (e.target.value = ""))} />
      <div className="flex flex-wrap items-center gap-2">
        <div className="relative min-w-48 flex-1">
          <Search className="pointer-events-none absolute left-2.5 top-2.5 size-4 text-muted-foreground" />
          <Input type="search" aria-label="Search references" placeholder="Search by name or type" className="pl-8" value={q} onChange={(e) => setQ(e.target.value)} />
        </div>
        <Select value={sort} onValueChange={(v) => setSort(v as SortKey)}>
          <SelectTrigger aria-label="Sort by" className="w-36">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="modified">Date added</SelectItem>
            <SelectItem value="name">Name</SelectItem>
            <SelectItem value="size">Size</SelectItem>
            <SelectItem value="type">Type</SelectItem>
          </SelectContent>
        </Select>
        {picked.size ? (
          <Button variant="outline" className="text-destructive" onClick={() => setConfirm([...picked])}>
            <Trash2 /> Delete {picked.size}
          </Button>
        ) : null}
        <Button onClick={() => input.current?.click()} disabled={upload.isPending || all.length >= MAX_FILES}>
          {upload.isPending ? <Spinner /> : <Upload />} Add references
        </Button>
      </div>

      {docs.isLoading ? (
        <ListSkeleton />
      ) : (
        <div className="border-t text-sm" role="table" aria-label="Your references">
          <div role="row" className="grid grid-cols-[1.5rem_1fr_auto] items-center gap-3 border-b py-2 text-xs text-muted-foreground md:grid-cols-[1.5rem_1fr_6rem_5rem_7rem_6rem]">
            <input type="checkbox" aria-label="Select all" checked={allPicked} onChange={() => setPicked(allPicked ? new Set() : new Set(shown.map((d) => d.name)))} className="size-4 accent-primary" />
            <div role="columnheader">{head("name", "Name")}</div>
            <div role="columnheader" className="hidden md:block">{head("type", "Type")}</div>
            <div role="columnheader" className="hidden md:block">{head("size", "Size")}</div>
            <div role="columnheader" className="hidden md:block">{head("modified", "Added")}</div>
            <div role="columnheader" className="sr-only md:not-sr-only md:text-right">Actions</div>
          </div>
          {shown.map((d) => (
            <div key={d.name} role="row" className="grid grid-cols-[1.5rem_1fr_auto] items-center gap-3 border-b py-2 hover:bg-muted/40 md:grid-cols-[1.5rem_1fr_6rem_5rem_7rem_6rem]">
              <input type="checkbox" aria-label={`Select ${d.name}`} checked={picked.has(d.name)} onChange={() => toggle(d.name)} className="size-4 accent-primary" />
              <div className="flex min-w-0 items-center gap-2" role="cell">
                <FileText className="size-4 shrink-0 text-muted-foreground" />
                <div className="min-w-0">
                  <div className="truncate font-medium" title={d.name}>{d.name}</div>
                  <div className="truncate font-mono text-[11px] text-muted-foreground md:hidden">{kind(d.name)} · {size(d.size)} · {ago(d.modified)}</div>
                </div>
              </div>
              <div className="hidden text-muted-foreground md:block" role="cell">{kind(d.name)}</div>
              <div className="hidden tabular-nums text-muted-foreground md:block" role="cell">{size(d.size)}</div>
              <div className="hidden text-muted-foreground md:block" role="cell" title={new Date(d.modified).toLocaleString()}>{ago(d.modified)}</div>
              <div className="flex justify-end" role="cell">
                <Button variant="ghost" size="icon" className="size-8" aria-label={`Copy the sandbox path of ${d.name}`} title={`Copy ${WORKSPACE}${d.name}`} onClick={() => void navigator.clipboard?.writeText(WORKSPACE + d.name).then(() => toast.success("Path copied"))}>
                  <Copy />
                </Button>
                <Button variant="ghost" size="icon" className="size-8" aria-label={`Download ${d.name}`} onClick={() => void downloadText(`/me/documents/${encodeURIComponent(d.name)}`, d.name).catch(toastError)}>
                  <Download />
                </Button>
                <Button variant="ghost" size="icon" className="size-8" aria-label={`Delete ${d.name}`} onClick={() => setConfirm([d.name])}>
                  <Trash2 />
                </Button>
              </div>
            </div>
          ))}
          {shown.length === 0 ? (
            <div className="py-10 text-center text-sm text-muted-foreground">
              {all.length ? "No reference matches your search." : "Nothing here yet. Drop files anywhere on this page, or use Add references: PDF, Word, text, spreadsheet, slides or image, up to 20 MB each."}
            </div>
          ) : null}
        </div>
      )}

      <p className="text-xs text-muted-foreground">
        {all.length} of {MAX_FILES} files · {size(total)} of {size(MAX_TOTAL)}. Agents see the list in every session, can copy a file to <span className="font-mono">{WORKSPACE}</span> to read it, and can attach it to a web form. Only you can see them.
      </p>

      {drag ? (
        <div className="pointer-events-none absolute -inset-2 z-10 flex items-center justify-center rounded-lg border-2 border-dashed border-primary bg-background/80 text-sm font-medium">Drop to add references</div>
      ) : null}

      <AlertDialog open={!!confirm} onOpenChange={(o) => !o && setConfirm(undefined)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete {confirm && confirm.length > 1 ? `${confirm.length} references` : confirm?.[0]}?</AlertDialogTitle>
            <AlertDialogDescription>Agents lose access to {confirm && confirm.length > 1 ? "them" : "it"} in every session. Copies already in a sandbox stay there until it is deleted.</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                for (const n of confirm ?? []) remove.mutate(n);
                setPicked(new Set());
              }}
            >
              Delete
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
