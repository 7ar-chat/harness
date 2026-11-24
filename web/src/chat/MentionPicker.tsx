// The @ picker of the chat composer: typing "@" at the end of the message lists the roles, the installed skills and, when the
// app already has them in its cache, the project's requirements and the user's documents. A role or a skill becomes a chip
// (the run is sent as that role / with that skill loaded); a requirement or a document is written into the message.
import { useQueryClient } from "@tanstack/react-query";
import { Bot, FileText, ListChecks, Sparkles, X } from "lucide-react";
import { useEffect, useMemo, useReducer, useState, type KeyboardEvent } from "react";
import { useCommands } from "@/api/queries";
import { useRoles } from "@/api/roles";
import { cn } from "@/lib/utils";

export type MentionKind = "role" | "skill" | "requirement" | "document";

export interface MentionItem {
  kind: MentionKind;
  /** The role or skill name; a requirement's id; a document's file name. */
  id: string;
  label: string;
  detail?: string;
}

const GROUPS: { kind: MentionKind; title: string }[] = [
  { kind: "role", title: "Roles" },
  { kind: "skill", title: "Skills" },
  { kind: "requirement", title: "Requirements" },
  { kind: "document", title: "Documents" },
];
const ICON = { role: Bot, skill: Sparkles, requirement: ListChecks, document: FileText } as const;

/** The "@query" the message ends with: it starts the message or follows a space, and holds no space. */
export function findMention(text: string): { start: number; query: string } | null {
  const m = /(^|\s)@([^\s@]*)$/.exec(text);
  if (!m) return null;
  return { start: m.index + (m[1] ?? "").length, query: m[2] ?? "" };
}

/** Items matching query: names that start with it first, then those that contain it, in the order roles, skills, requirements, documents. */
export function filterMentions(items: MentionItem[], query: string, limit = 8): MentionItem[] {
  const q = query.toLowerCase();
  const score = (i: MentionItem) => {
    const hay = [i.id.toLowerCase(), i.label.toLowerCase()];
    if (!q) return 1;
    if (hay.some((h) => h.startsWith(q))) return 0;
    return hay.some((h) => h.includes(q)) || (i.detail ?? "").toLowerCase().includes(q) ? 1 : 2;
  };
  const order = (i: MentionItem) => GROUPS.findIndex((g) => g.kind === i.kind);
  return items
    .filter((i) => score(i) < 2)
    .sort((a, b) => order(a) - order(b) || score(a) - score(b) || a.id.localeCompare(b.id))
    .slice(0, limit);
}

/** What the message becomes when item is picked: a chip item leaves no text behind, a reference is written in place of "@query". */
export function applyMention(text: string, at: { start: number }, item: MentionItem): string {
  const head = text.slice(0, at.start);
  switch (item.kind) {
    case "requirement":
      return `${head}Requirement ${item.id}${item.label && item.label !== item.id ? ` (${item.label})` : ""} `;
    case "document":
      return `${head}/workspace/documents/${item.id} `;
    default:
      return head;
  }
}

/** Roles and skills travel as settings of the run (a chip above the input); the others are written into the message. */
export const isChip = (i: MentionItem) => i.kind === "role" || i.kind === "skill";

/** Picks a chip: a second role replaces the first, a skill is added once. */
export function addChip(chips: MentionItem[], item: MentionItem): MentionItem[] {
  if (item.kind === "role") return [...chips.filter((c) => c.kind !== "role"), item];
  return chips.some((c) => c.kind === item.kind && c.id === item.id) ? chips : [...chips, item];
}

interface Cached {
  id?: unknown;
  title?: unknown;
  name?: unknown;
}

/** Items of a query the app has already loaded (never fetches): lists of objects with an id and a title or name. */
function cachedItems(entries: [readonly unknown[], unknown][], kind: "requirement" | "document"): MentionItem[] {
  const out: MentionItem[] = [];
  for (const [, data] of entries) {
    const list = Array.isArray(data) ? data : data && typeof data === "object" && Array.isArray((data as { items?: unknown }).items) ? (data as { items: unknown[] }).items : [];
    for (const raw of list) {
      if (!raw || typeof raw !== "object") continue;
      const o = raw as Cached;
      const title = typeof o.title === "string" ? o.title : typeof o.name === "string" ? o.name : "";
      if (kind === "document") {
        if (typeof o.name === "string") out.push({ kind, id: o.name, label: o.name });
      } else if (typeof o.id === "string" || typeof o.id === "number") {
        out.push({ kind, id: String(o.id), label: title || String(o.id), detail: title ? String(o.id) : undefined });
      }
    }
  }
  return out;
}

/** Everything the picker can offer in this chat: enabled roles, installed skills, and the cached requirements and documents. */
export function useMentionItems(projectId?: string): MentionItem[] {
  const roles = useRoles(projectId).data;
  const commands = useCommands().data;
  const qc = useQueryClient();
  const own = useMemo(() => {
    const items: MentionItem[] = [];
    for (const r of roles ?? []) if (r.enabled) items.push({ kind: "role", id: r.name, label: r.title || r.name, detail: r.description });
    for (const c of commands ?? []) if (c.source === "skill") items.push({ kind: "skill", id: c.name, label: c.name, detail: c.description });
    return items;
  }, [roles, commands]);
  // The lists other pages loaded (a tab that stays open in the background) show up here without a refetch: read at every
  // render, and render again when one of those two lists changes in the cache.
  const [, refresh] = useReducer((n: number) => n + 1, 0);
  useEffect(
    () =>
      qc.getQueryCache().subscribe((e) => {
        const k = e.query.queryKey[0];
        if (k === "requirements" || k === "documents") refresh();
      }),
    [qc],
  );
  return [...own, ...cachedItems(qc.getQueriesData({ queryKey: ["requirements"] }), "requirement"), ...cachedItems(qc.getQueriesData({ queryKey: ["documents"] }), "document")];
}

/** The picker's state for a composer holding `text`: the matches, the highlighted one, and what picking or the keys do. */
export function useMention(text: string, items: MentionItem[], chips: MentionItem[], onChips: (next: MentionItem[]) => void, setText: (t: string) => void) {
  const at = findMention(text);
  const [index, setIndex] = useState(0);
  const [closed, setClosed] = useState<string | null>(null);
  const taken = (i: MentionItem) => isChip(i) && chips.some((c) => c.kind === i.kind && c.id === i.id);
  const list = at && closed !== text ? filterMentions(items.filter((i) => !taken(i)), at.query) : [];
  const active = Math.min(index, Math.max(list.length - 1, 0));
  const pick = (item: MentionItem) => {
    if (!at) return;
    setText(applyMention(text, at, item));
    if (isChip(item)) onChips(addChip(chips, item));
    setIndex(0);
  };
  /** Capture-phase key handler of the composer: moves and picks while the list is open, else leaves the keys alone. */
  const onKeyDownCapture = (e: KeyboardEvent<HTMLElement>) => {
    if (!list.length) return;
    const stop = () => {
      e.preventDefault();
      e.stopPropagation();
    };
    if (e.key === "ArrowDown") {
      stop();
      setIndex((active + 1) % list.length);
    } else if (e.key === "ArrowUp") {
      stop();
      setIndex((active - 1 + list.length) % list.length);
    } else if ((e.key === "Enter" && !e.shiftKey) || e.key === "Tab") {
      const it = list[active];
      if (it) {
        stop();
        pick(it);
      }
    } else if (e.key === "Escape") {
      stop();
      setClosed(text);
    }
  };
  return { list, active, setIndex, pick, onKeyDownCapture };
}

/** The list under the composer. `active` is the highlighted index of `items`, as the keyboard moves it. */
export function MentionPicker({ items, active, onPick, onHover }: { items: MentionItem[]; active: number; onPick: (i: MentionItem) => void; onHover?: (index: number) => void }) {
  if (!items.length) return null;
  return (
    <div className="mb-2 max-h-72 overflow-y-auto rounded-md border bg-popover text-sm shadow-sm" role="listbox" aria-label="Mentions">
      {GROUPS.map((g) => {
        const rows = items.map((it, index) => ({ it, index })).filter((x) => x.it.kind === g.kind);
        if (!rows.length) return null;
        return (
          <div key={g.kind} role="group" aria-label={g.title}>
            <div className="px-3 pb-0.5 pt-1.5 text-[11px] font-medium uppercase tracking-wide text-muted-foreground">{g.title}</div>
            {rows.map(({ it, index }) => {
              const Icon = ICON[it.kind];
              return (
                <button
                  key={`${it.kind}:${it.id}`}
                  type="button"
                  role="option"
                  aria-selected={index === active}
                  className={cn("flex w-full items-center gap-2 px-3 py-1.5 text-left hover:bg-accent", index === active && "bg-accent")}
                  onMouseEnter={() => onHover?.(index)}
                  onMouseDown={(e) => e.preventDefault()}
                  onClick={() => onPick(it)}
                >
                  <Icon className="size-3.5 shrink-0 text-muted-foreground" />
                  <code>{it.kind === "role" || it.kind === "skill" ? `@${it.id}` : it.id}</code>
                  <span className="truncate text-xs text-muted-foreground">{it.kind === "role" && it.label !== it.id ? `${it.label}${it.detail ? ` · ${it.detail}` : ""}` : it.detail ?? (it.label !== it.id ? it.label : "")}</span>
                </button>
              );
            })}
          </div>
        );
      })}
    </div>
  );
}

/** The roles and skills picked for the next message, shown above the input. */
export function MentionChips({ chips, onRemove }: { chips: MentionItem[]; onRemove: (i: MentionItem) => void }) {
  if (!chips.length) return null;
  return (
    <div className="mb-2 flex flex-wrap gap-1.5" aria-label="Picked">
      {chips.map((c) => {
        const Icon = ICON[c.kind];
        return (
          <span key={`${c.kind}:${c.id}`} className="inline-flex items-center gap-1 rounded-full bg-primary/10 px-2 py-0.5 text-xs">
            <Icon className="size-3" />
            {c.kind === "role" ? `Run as @${c.id}` : `Skill ${c.id}`}
            <button type="button" aria-label={`Remove ${c.id}`} className="rounded-full hover:bg-primary/20" onClick={() => onRemove(c)}>
              <X className="size-3" />
            </button>
          </span>
        );
      })}
    </div>
  );
}
