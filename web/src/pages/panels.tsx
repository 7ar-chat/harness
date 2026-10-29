// Panels of the session view's right-hand side: files, plan, project memory, activity.
import { formatDistanceToNowStrict } from "date-fns";
import { CheckCircle2, Circle, CircleDot, Download, File as FileIcon, Folder, FolderOpen } from "lucide-react";
import { useCallback, useEffect, useState } from "react";
import { NodeApi, Tree, type NodeRendererProps } from "react-arborist";
import { api, downloadFile, authHeaders } from "@/api/client";
import { toastError, useAudit } from "@/api/queries";
import type { PlanItem, WorkspaceFile } from "@/api/types";
import { Empty, ListSkeleton } from "@/components/app/common";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { useElementSize } from "@/lib/hooks";

interface TreeNode {
  id: string;
  name: string;
  path: string;
  size: number;
  isDir: boolean;
  children?: TreeNode[] | null;
  loaded?: boolean;
}

const toNode = (f: WorkspaceFile): TreeNode => ({ id: f.path, name: f.name, path: f.path, size: f.size, isDir: f.type === "directory", children: f.type === "directory" ? [] : undefined });

function setChildren(nodes: TreeNode[], path: string, kids: TreeNode[]): TreeNode[] {
  return nodes.map((n) => (n.path === path ? { ...n, children: kids, loaded: true } : n.children ? { ...n, children: setChildren(n.children, path, kids) } : n));
}

function Row({ node, style }: NodeRendererProps<TreeNode>) {
  const d = node.data;
  return (
    <div style={style} className={`flex cursor-pointer items-center gap-1.5 rounded px-1 text-sm hover:bg-accent ${node.isSelected ? "bg-accent" : ""}`} onClick={() => (d.isDir ? node.toggle() : node.activate())}>
      {d.isDir ? node.isOpen ? <FolderOpen className="size-4 shrink-0" /> : <Folder className="size-4 shrink-0" /> : <FileIcon className="size-4 shrink-0 text-muted-foreground" />}
      <span className="truncate">{d.name}</span>
      {!d.isDir ? <span className="ml-auto shrink-0 text-xs text-muted-foreground">{d.size < 1024 ? `${d.size} B` : `${Math.ceil(d.size / 1024)} KB`}</span> : null}
    </div>
  );
}

const isImage = (n: string) => /\.(png|jpe?g|gif|webp|svg|bmp)$/i.test(n);

function Preview({ threadId, node, onClose }: { threadId: string; node: TreeNode | null; onClose: () => void }) {
  const [state, setState] = useState<{ text?: string; img?: string; err?: string }>({});
  useEffect(() => {
    setState({});
    if (!node) return;
    let url: string | undefined;
    let alive = true;
    (async () => {
      try {
        const r = await fetch(`/api/threads/${threadId}/files/download?path=${encodeURIComponent(node.path)}`, { headers: authHeaders() });
        if (!r.ok) throw new Error(`HTTP ${r.status}`);
        if (isImage(node.name)) {
          url = URL.createObjectURL(await r.blob());
          if (alive) setState({ img: url });
        } else if (node.size > 300_000) {
          if (alive) setState({ err: "Too large to preview. Download it instead." });
        } else {
          const t = await r.text();
          if (alive) setState(t.includes("\u0000") ? { err: "Binary file. Download it instead." } : { text: t });
        }
      } catch (e) {
        if (alive) setState({ err: e instanceof Error ? e.message : "Failed to load" });
      }
    })();
    return () => {
      alive = false;
      if (url) URL.revokeObjectURL(url);
    };
  }, [node, threadId]);
  return (
    <Dialog open={!!node} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-w-3xl">
        <DialogHeader>
          <DialogTitle className="truncate pr-6">{node?.name}</DialogTitle>
          <DialogDescription className="truncate">{node?.path}</DialogDescription>
        </DialogHeader>
        <div className="max-h-[60vh] overflow-auto rounded-md border bg-muted/40 p-2">
          {state.img ? <img src={state.img} alt={node?.name} className="max-w-full" /> : state.err ? <p className="p-2 text-sm text-muted-foreground">{state.err}</p> : state.text !== undefined ? <pre className="whitespace-pre-wrap break-words text-xs">{state.text}</pre> : <ListSkeleton rows={3} />}
        </div>
        <Button variant="outline" className="self-start" onClick={() => node && downloadFile(threadId, node.path, node.name).catch(toastError)}>
          <Download /> Download
        </Button>
      </DialogContent>
    </Dialog>
  );
}

export function FilesPanel({ threadId, refreshKey }: { threadId: string; refreshKey: unknown }) {
  const [nodes, setNodes] = useState<TreeNode[] | null>(null);
  const [err, setErr] = useState<string>();
  const [preview, setPreview] = useState<TreeNode | null>(null);
  const [ref, size] = useElementSize<HTMLDivElement>();
  const load = useCallback(
    async (path: string) => (await api.get<WorkspaceFile[]>(`/threads/${threadId}/files?path=${encodeURIComponent(path)}`)).map(toNode),
    [threadId],
  );
  useEffect(() => {
    let alive = true;
    setErr(undefined);
    load("")
      .then((n) => alive && setNodes(n))
      .catch((e: unknown) => alive && setErr(e instanceof Error ? e.message : "Failed to load files"));
    return () => {
      alive = false;
    };
  }, [load, refreshKey]);
  if (err) return <Empty title="No workspace yet">{err}</Empty>;
  if (!nodes) return <ListSkeleton rows={3} />;
  if (!nodes.length) return <Empty title="The workspace is empty">Files the agent creates appear here.</Empty>;
  return (
    <div ref={ref} className="h-full min-h-40 p-2">
      {size.height > 0 ? (
        <Tree<TreeNode>
          data={nodes}
          width={size.width}
          height={size.height}
          rowHeight={28}
          indent={14}
          openByDefault={false}
          disableDrag
          disableEdit
          disableMultiSelection
          childrenAccessor={(d) => d.children ?? null}
          onToggle={(id) => {
            const n = findNode(nodes, id);
            if (n && n.isDir && !n.loaded) void load(n.path).then((kids) => setNodes((cur) => (cur ? setChildren(cur, n.path, kids) : cur))).catch(toastError);
          }}
          onActivate={(node: NodeApi<TreeNode>) => !node.data.isDir && setPreview(node.data)}
        >
          {Row}
        </Tree>
      ) : null}
      <Preview threadId={threadId} node={preview} onClose={() => setPreview(null)} />
    </div>
  );
}

function findNode(nodes: TreeNode[], id: string): TreeNode | undefined {
  for (const n of nodes) {
    if (n.id === id) return n;
    const f = n.children && findNode(n.children, id);
    if (f) return f;
  }
  return undefined;
}

export function PlanPanel({ items }: { items: PlanItem[] }) {
  if (!items.length) return <Empty title="No plan yet">When the agent plans its work, the steps show up here.</Empty>;
  return (
    <ol className="space-y-2 p-3">
      {items.map((p) => (
        <li key={p.id} className="flex items-start gap-2 text-sm">
          {p.status === "completed" ? <CheckCircle2 className="mt-0.5 size-4 shrink-0 text-success" /> : p.status === "in_progress" ? <CircleDot className="mt-0.5 size-4 shrink-0 text-warning" /> : <Circle className="mt-0.5 size-4 shrink-0 text-muted-foreground" />}
          <span className={p.status === "completed" ? "text-muted-foreground line-through" : undefined}>{p.title}</span>
        </li>
      ))}
    </ol>
  );
}

export function ActivityPanel({ projectId, threadId }: { projectId?: string; threadId: string }) {
  const audit = useAudit(projectId ?? "", 200);
  if (!projectId) return <Empty title="Personal session">Activity is recorded for project sessions.</Empty>;
  if (audit.isLoading) return <ListSkeleton rows={4} />;
  const rows = (audit.data ?? []).filter((a) => a.threadId === threadId);
  if (!rows.length) return <Empty title="No activity yet" />;
  return (
    <ul className="divide-y">
      {rows.map((a) => (
        <li key={a.id} className="px-3 py-2 text-sm">
          <div>
            <span className="font-medium">{a.actor}</span>
            {a.onBehalfOf ? <span className="text-muted-foreground"> (for {a.onBehalfOf})</span> : null} <span className="text-muted-foreground">{a.action}</span>
          </div>
          <div className="text-xs text-muted-foreground">{formatDistanceToNowStrict(new Date(a.at))} ago</div>
        </li>
      ))}
    </ul>
  );
}
