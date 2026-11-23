// Artifacts published into an answer (the agent's publish_artifact tool): an inline card in the chat with a preview
// (HTML, markdown, images, PDF, CSV), the source, and a download; and the thread's list of them in the Files tab.
import { useQuery } from "@tanstack/react-query";
import { ChevronDown, ChevronRight, Download, FileText, Package } from "lucide-react";
import { createContext, useContext, useEffect, useState } from "react";
import Markdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { api, authHeaders, downloadFile } from "@/api/client";
import { toastError } from "@/api/queries";
import type { PublishedArtifact } from "@/api/types";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { cn } from "@/lib/utils";

/** What the card needs of a published artifact (the tool result has no run or timestamps beyond these). */
export type PublishedItem = Pick<PublishedArtifact, "artifactId" | "path" | "name" | "version" | "mimeType" | "sizeBytes"> & { title?: string };

/** The thread the artifact cards of the chat belong to. */
export const ArtifactContext = createContext<string | undefined>(undefined);

export type ArtifactKind = "html" | "markdown" | "image" | "pdf" | "csv" | "text" | "other";

export function artifactKind(name: string, mime = ""): ArtifactKind {
  const ext = /\.([a-z0-9]+)$/i.exec(name)?.[1]?.toLowerCase() ?? "";
  if (ext === "html" || ext === "htm" || mime === "text/html") return "html";
  if (ext === "md" || ext === "markdown" || mime === "text/markdown") return "markdown";
  if (["png", "jpg", "jpeg", "gif", "webp", "svg"].includes(ext) || mime.startsWith("image/")) return "image";
  if (ext === "pdf" || mime === "application/pdf") return "pdf";
  if (ext === "csv" || mime === "text/csv") return "csv";
  if (mime.startsWith("text/") || ["txt", "json", "yaml", "yml", "xml", "js", "ts", "tsx", "py", "go", "css", "sh", "log", "toml"].includes(ext)) return "text";
  return "other";
}

/** The artifact a publish_artifact tool result describes, null for anything else (a refused call, a partial result). */
export function publishedFromResult(result: unknown): PublishedItem | null {
  if (typeof result !== "string") return null;
  try {
    const p = (JSON.parse(result) as { published?: Partial<PublishedItem> }).published;
    if (p && typeof p.path === "string" && typeof p.artifactId === "string") {
      return { artifactId: p.artifactId, path: p.path, name: p.name || p.path.split("/").pop() || p.path, title: p.title, version: p.version ?? 1, mimeType: p.mimeType ?? "", sizeBytes: p.sizeBytes ?? 0 };
    }
  } catch {
    /* not JSON: the call failed with a message for the model */
  }
  return null;
}

const previewUrl = (threadId: string, path: string) => `/api/threads/${threadId}/files/preview?path=${encodeURIComponent(path)}`;
const MAX_TEXT = 1_000_000;

function useText(threadId: string, path: string, enabled: boolean) {
  const [state, setState] = useState<{ text?: string; err?: string }>({});
  useEffect(() => {
    if (!enabled) return;
    let alive = true;
    setState({});
    (async () => {
      try {
        const r = await fetch(`/api/threads/${threadId}/files/download?path=${encodeURIComponent(path)}`, { headers: authHeaders() });
        if (!r.ok) throw new Error(`HTTP ${r.status}`);
        const t = await r.text();
        if (!alive) return;
        if (t.includes("\u0000")) setState({ err: "Binary file. Download it instead." });
        else setState({ text: t.length > MAX_TEXT ? `${t.slice(0, MAX_TEXT)}\n… (truncated)` : t });
      } catch (e) {
        if (alive) setState({ err: e instanceof Error ? e.message : "Failed to load" });
      }
    })();
    return () => {
      alive = false;
    };
  }, [threadId, path, enabled]);
  return state;
}

/** A small RFC 4180 reader: quoted fields, doubled quotes, newlines inside quotes. */
export function parseCsv(text: string, maxRows = 200): string[][] {
  const rows: string[][] = [];
  let row: string[] = [];
  let cell = "";
  let quoted = false;
  for (let i = 0; i < text.length && rows.length <= maxRows; i++) {
    const c = text[i];
    if (quoted) {
      if (c === '"' && text[i + 1] === '"') (cell += '"', i++);
      else if (c === '"') quoted = false;
      else cell += c;
    } else if (c === '"') quoted = true;
    else if (c === ",") (row.push(cell), (cell = ""));
    else if (c === "\n" || c === "\r") {
      if (c === "\r" && text[i + 1] === "\n") i++;
      row.push(cell);
      rows.push(row);
      row = [];
      cell = "";
    } else cell += c;
  }
  if (cell || row.length) rows.push([...row, cell]);
  return rows.slice(0, maxRows + 1);
}

function CsvTable({ text }: { text: string }) {
  const rows = parseCsv(text);
  if (!rows.length) return <p className="p-3 text-sm text-muted-foreground">Empty file.</p>;
  const [head, ...body] = rows;
  return (
    <table className="w-full border-collapse text-xs">
      <thead className="sticky top-0 bg-muted">
        <tr>
          {head?.map((h, i) => (
            <th key={i} className="border-b px-2 py-1 text-left font-medium">
              {h}
            </th>
          ))}
        </tr>
      </thead>
      <tbody>
        {body.map((r, i) => (
          <tr key={i} className="odd:bg-muted/30">
            {r.map((c, j) => (
              <td key={j} className="border-b px-2 py-1 align-top">
                {c}
              </td>
            ))}
          </tr>
        ))}
      </tbody>
    </table>
  );
}

const sizeLabel = (n: number) => (n < 1024 ? `${n} B` : n < 1024 * 1024 ? `${Math.ceil(n / 1024)} KB` : `${(n / 1024 / 1024).toFixed(1)} MB`);

/** The content of an artifact: a preview when its type has one, else (or on request) its source. */
export function ArtifactViewer({ threadId, pub, view, height = "h-80" }: { threadId: string; pub: PublishedItem; view: "preview" | "source"; height?: string }) {
  const kind = artifactKind(pub.name, pub.mimeType);
  const binary = kind === "image" || kind === "pdf";
  const framed = view === "preview" && (kind === "html" || binary);
  const { text, err } = useText(threadId, pub.path, !binary && !framed);
  const frame = cn("w-full rounded-md border bg-white", height);
  if (view === "preview") {
    if (kind === "html") return <iframe title={pub.title || pub.name} src={previewUrl(threadId, pub.path)} sandbox="allow-scripts allow-popups" className={frame} />;
    if (kind === "pdf") return <iframe title={pub.title || pub.name} src={previewUrl(threadId, pub.path)} className={frame} />;
    if (kind === "image") return <img src={previewUrl(threadId, pub.path)} alt={pub.title || pub.name} className="max-h-80 max-w-full rounded-md border bg-white object-contain" />;
  }
  if (err) return <p className="p-2 text-sm text-muted-foreground">{err}</p>;
  if (binary) return <p className="p-2 text-sm text-muted-foreground">Binary file. Download it instead.</p>;
  if (text === undefined) return <p className="p-2 text-sm text-muted-foreground">Loading…</p>;
  const box = cn("overflow-auto rounded-md border bg-card", height);
  if (view === "preview" && kind === "markdown")
    return (
      <div className={cn(box, "md p-3")}>
        <Markdown remarkPlugins={[remarkGfm]}>{text}</Markdown>
      </div>
    );
  if (view === "preview" && kind === "csv") return <div className={box}><CsvTable text={text} /></div>;
  return <pre className={cn(box, "whitespace-pre-wrap break-words bg-muted p-2 text-xs")}>{text}</pre>;
}

const hasPreview = (k: ArtifactKind) => k === "html" || k === "markdown" || k === "image" || k === "pdf" || k === "csv";

/** The inline card of a published artifact. */
export function ArtifactCard({ pub }: { pub: PublishedItem }) {
  const threadId = useContext(ArtifactContext);
  const kind = artifactKind(pub.name, pub.mimeType);
  const [open, setOpen] = useState(true);
  const [view, setView] = useState<"preview" | "source">(hasPreview(kind) ? "preview" : "source");
  if (!threadId) return null;
  return (
    <figure className="my-2 rounded-lg border bg-card text-card-foreground" aria-label={`Artifact ${pub.title || pub.name}`}>
      <figcaption className="flex flex-wrap items-center gap-2 px-3 py-2 text-sm">
        <button type="button" className="flex min-w-0 items-center gap-2 text-left" onClick={() => setOpen(!open)} aria-expanded={open}>
          {open ? <ChevronDown className="size-4 shrink-0" /> : <ChevronRight className="size-4 shrink-0" />}
          <FileText className="size-4 shrink-0 text-muted-foreground" />
          <span className="truncate font-medium">{pub.title || pub.name}</span>
        </button>
        <span className="truncate text-xs text-muted-foreground">
          {pub.title && pub.title !== pub.name ? `${pub.name} · ` : ""}
          {sizeLabel(pub.sizeBytes)}
        </span>
        <div className="ml-auto flex items-center gap-1">
          {hasPreview(kind) ? (
            <div className="flex rounded-md bg-muted p-0.5 text-xs" role="tablist" aria-label="View">
              {(["preview", "source"] as const).map((v) => (
                <button key={v} type="button" role="tab" aria-selected={view === v} className={cn("rounded px-2 py-0.5 capitalize", view === v && "bg-background shadow-sm")} onClick={() => (setView(v), setOpen(true))}>
                  {v}
                </button>
              ))}
            </div>
          ) : null}
          <Button type="button" variant="ghost" size="sm" className="h-7 px-2 text-xs" onClick={() => downloadFile(threadId, pub.path, pub.name).catch(toastError)}>
            <Download className="size-3" /> Download
          </Button>
        </div>
      </figcaption>
      {open ? (
        <div className="px-3 pb-3">
          <ArtifactViewer threadId={threadId} pub={pub} view={view} />
        </div>
      ) : null}
    </figure>
  );
}

export const publishedKey = (threadId: string) => ["published", threadId] as const;

/** The artifacts the agent published on the thread, oldest first. */
export function usePublished(threadId: string, refreshKey?: unknown) {
  return useQuery({ queryKey: [...publishedKey(threadId), refreshKey ?? null], queryFn: () => api.get<PublishedArtifact[]>(`/threads/${threadId}/published`), retry: false });
}

/** The per-thread Artifacts list of the Files tab. */
export function PublishedList({ threadId, refreshKey }: { threadId: string; refreshKey?: unknown }) {
  const q = usePublished(threadId, refreshKey);
  const [open, setOpen] = useState<PublishedArtifact | null>(null);
  const [view, setView] = useState<"preview" | "source">("preview");
  const list = q.data ?? [];
  if (!list.length) return null;
  return (
    <section aria-label="Artifacts" className="border-b p-2">
      <h3 className="mb-1 flex items-center gap-1.5 px-1 text-xs font-medium text-muted-foreground">
        <Package className="size-3.5" /> Artifacts ({list.length})
      </h3>
      <ul className="space-y-0.5">
        {[...list].reverse().map((a) => (
          <li key={a.artifactId}>
            <button
              type="button"
              className="flex w-full items-center gap-2 rounded px-1.5 py-1 text-left text-sm hover:bg-accent"
              onClick={() => (setView(hasPreview(artifactKind(a.name, a.mimeType)) ? "preview" : "source"), setOpen(a))}
            >
              <FileText className="size-4 shrink-0 text-muted-foreground" />
              <span className="truncate">{a.title || a.name}</span>
              <span className="ml-auto shrink-0 text-xs text-muted-foreground">{sizeLabel(a.sizeBytes)}</span>
            </button>
          </li>
        ))}
      </ul>
      <Dialog open={!!open} onOpenChange={(o) => !o && setOpen(null)}>
        <DialogContent className="max-w-3xl">
          <DialogHeader>
            <DialogTitle className="truncate pr-6">{open?.title || open?.name}</DialogTitle>
            <DialogDescription className="truncate">{open?.path}</DialogDescription>
          </DialogHeader>
          {open ? (
            <>
              {hasPreview(artifactKind(open.name, open.mimeType)) ? (
                <div className="flex w-fit rounded-md bg-muted p-0.5 text-xs" role="tablist" aria-label="View">
                  {(["preview", "source"] as const).map((v) => (
                    <button key={v} type="button" role="tab" aria-selected={view === v} className={cn("rounded px-2 py-0.5 capitalize", view === v && "bg-background shadow-sm")} onClick={() => setView(v)}>
                      {v}
                    </button>
                  ))}
                </div>
              ) : null}
              <ArtifactViewer threadId={threadId} pub={open} view={view} height="h-[55vh]" />
              <Button variant="outline" className="self-start" onClick={() => downloadFile(threadId, open.path, open.name).catch(toastError)}>
                <Download /> Download
              </Button>
            </>
          ) : null}
        </DialogContent>
      </Dialog>
    </section>
  );
}
