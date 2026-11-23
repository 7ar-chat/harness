// What the agents consumed: tokens (cache reads and writes apart), what it cost, by kind of work, and the monthly cap.
import { Link } from "@tanstack/react-router";
import { AlertTriangle } from "lucide-react";
import { useEffect, useState } from "react";
import { isUnsupported } from "@/api/client";
import { useSetUsageCap, useUsage, type UsageGroup } from "@/api/schedulesUsage";
import type { UsageCap, UsageRow, UsageTotals, UsageView } from "@/api/types";
import { Empty, ErrorState, ListSkeleton } from "@/components/app/common";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { cn } from "@/lib/utils";

const RANGES: { key: string; label: string }[] = [
  { key: "day", label: "Last 24 hours" },
  { key: "week", label: "Last 7 days" },
  { key: "month", label: "This month" },
  { key: "custom", label: "Custom" },
];
const GROUPS: { key: UsageGroup; label: string }[] = [
  { key: "day", label: "By day" },
  { key: "model", label: "By model" },
  { key: "thread", label: "By session" },
  { key: "user", label: "By person" },
];
const KINDS: { key: string; label: string }[] = [
  { key: "session", label: "Interactive sessions" },
  { key: "task", label: "Factory tasks" },
  { key: "schedule", label: "Scheduled tasks" },
];

export const money = (usd: number) => (usd >= 100 ? `$${usd.toFixed(0)}` : usd >= 1 ? `$${usd.toFixed(2)}` : usd === 0 ? "$0" : `$${usd.toFixed(4)}`);
export const tokens = (n: number) => (n >= 1e9 ? `${(n / 1e9).toFixed(2)}B` : n >= 1e6 ? `${(n / 1e6).toFixed(2)}M` : n >= 1e4 ? `${(n / 1e3).toFixed(1)}k` : n.toLocaleString());

/** The share of the input served from the provider's cache, 0 to 1. */
export const cacheHit = (t: UsageTotals) => (t.inputTokens > 0 ? t.cacheReadTokens / t.inputTokens : 0);

function Tile({ label, value, hint }: { label: string; value: string; hint?: string }) {
  return (
    <div className="rounded-lg border p-3">
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className="mt-1 text-xl font-semibold tabular-nums">{value}</div>
      {hint ? <div className="mt-0.5 text-xs text-muted-foreground">{hint}</div> : null}
    </div>
  );
}

/** Cost per day as bars; each bar names its day and amount for a pointer and for a screen reader (the table below carries the same numbers). */
export function DayChart({ rows }: { rows: UsageRow[] }) {
  if (!rows.length) return null;
  const W = 640;
  const H = 150;
  const pad = { l: 8, r: 8, t: 8, b: 22 };
  const max = Math.max(...rows.map((r) => r.costUsd), 1e-9);
  const bw = (W - pad.l - pad.r) / rows.length;
  return (
    <figure className="rounded-lg border p-3">
      <figcaption className="mb-1 flex items-baseline justify-between text-xs text-muted-foreground">
        <span>Cost per day</span>
        <span>peak {money(max)}</span>
      </figcaption>
      <svg viewBox={`0 0 ${W} ${H}`} role="img" aria-label={`Cost per day, ${rows.length} days, peak ${money(max)}`} className="h-36 w-full">
        <line x1={pad.l} x2={W - pad.r} y1={H - pad.b} y2={H - pad.b} className="stroke-border" />
        {rows.map((r, i) => {
          const h = Math.max(r.costUsd > 0 ? 1.5 : 0, (r.costUsd / max) * (H - pad.t - pad.b));
          const x = pad.l + i * bw;
          return (
            <rect key={r.key} x={x + bw * 0.15} y={H - pad.b - h} width={bw * 0.7} height={h} rx={2} className="fill-primary">
              <title>{`${r.key}: ${money(r.costUsd)}, ${r.runs} runs`}</title>
            </rect>
          );
        })}
        <text x={pad.l} y={H - 6} className="fill-muted-foreground text-[10px]">
          {rows[0]!.key}
        </text>
        {rows.length > 1 ? (
          <text x={W - pad.r} y={H - 6} textAnchor="end" className="fill-muted-foreground text-[10px]">
            {rows[rows.length - 1]!.key}
          </text>
        ) : null}
      </svg>
    </figure>
  );
}

/** Where the tokens went: fresh input, input served from the cache, input written to it, and output. */
export function TokenSplit({ total }: { total: UsageTotals }) {
  const fresh = Math.max(0, total.inputTokens - total.cacheReadTokens - total.cacheWriteTokens);
  const parts = [
    { key: "fresh", label: "Fresh input", n: fresh, cls: "bg-primary" },
    { key: "read", label: "Cache read", n: total.cacheReadTokens, cls: "bg-success" },
    { key: "write", label: "Cache write", n: total.cacheWriteTokens, cls: "bg-warning" },
    { key: "out", label: "Output", n: total.outputTokens, cls: "bg-muted-foreground" },
  ];
  const sum = parts.reduce((s, p) => s + p.n, 0);
  if (sum === 0) return null;
  return (
    <div className="space-y-2 rounded-lg border p-3">
      <div className="flex h-3 overflow-hidden rounded-full bg-muted" role="img" aria-label={parts.map((p) => `${p.label} ${Math.round((p.n / sum) * 100)}%`).join(", ")}>
        {parts.map((p) => (
          <div key={p.key} className={p.cls} style={{ width: `${(p.n / sum) * 100}%` }} />
        ))}
      </div>
      <ul className="grid gap-x-4 gap-y-1 text-xs sm:grid-cols-4">
        {parts.map((p) => (
          <li key={p.key} className="flex items-center gap-1.5">
            <span className={cn("size-2 rounded-full", p.cls)} aria-hidden />
            <span className="text-muted-foreground">{p.label}</span>
            <span className="ml-auto tabular-nums">{tokens(p.n)}</span>
          </li>
        ))}
      </ul>
    </div>
  );
}

/** The monthly cap: the bar, the warning from 80% and, for whoever may, the field that changes it. */
export function CapCard({ cap, canSet, projectId }: { cap: UsageCap; canSet: boolean; projectId?: string }) {
  const set = useSetUsageCap(projectId);
  const [v, setV] = useState(String(cap.monthlyUsd || ""));
  useEffect(() => setV(String(cap.monthlyUsd || "")), [cap.monthlyUsd]);
  const amount = Number(v);
  const valid = v.trim() !== "" && Number.isFinite(amount) && amount >= 0;
  const pct = Math.min(100, Math.round(cap.ratio * 100));
  return (
    <section className="space-y-3 rounded-lg border p-3" aria-label="Monthly cap">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h2 className="text-sm font-medium">Monthly cap{projectId ? " of the project" : ""}</h2>
        <span className="text-sm tabular-nums text-muted-foreground">
          {money(cap.spentUsd)} spent this month{cap.monthlyUsd > 0 ? ` of ${money(cap.monthlyUsd)}` : ", no cap"}
        </span>
      </div>
      {cap.monthlyUsd > 0 ? (
        <div className="h-2 overflow-hidden rounded-full bg-muted" role="progressbar" aria-label="Share of the monthly cap spent" aria-valuemin={0} aria-valuemax={100} aria-valuenow={pct}>
          <div className={cn("h-full", cap.reached ? "bg-destructive" : cap.warn ? "bg-warning" : "bg-primary")} style={{ width: `${pct}%` }} />
        </div>
      ) : null}
      {cap.reached ? (
        <p role="alert" className="flex items-start gap-2 text-sm text-destructive">
          <AlertTriangle className="mt-0.5 size-4 shrink-0" aria-hidden />
          The cap is used up: new runs are refused until the 1st of next month{canSet ? " or until you raise it" : projectId ? ", unless an owner raises it" : ""}.
        </p>
      ) : cap.warn ? (
        <p role="status" className="flex items-start gap-2 text-sm text-warning-foreground dark:text-warning">
          <AlertTriangle className="mt-0.5 size-4 shrink-0" aria-hidden />
          {pct}% of the monthly cap is spent. Runs are refused once it is used up.
        </p>
      ) : null}
      {canSet ? (
        <form
          className="flex flex-wrap items-end gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            if (valid) set.mutate(amount);
          }}
        >
          <div className="space-y-1">
            <Label htmlFor="cap-usd">Cap (USD per month, empty or 0: none)</Label>
            <Input id="cap-usd" type="number" min={0} step="any" value={v} onChange={(e) => setV(e.target.value)} className="w-40" placeholder="no cap" />
          </div>
          <Button type="submit" disabled={!valid || set.isPending}>
            Save cap
          </Button>
        </form>
      ) : null}
    </section>
  );
}

function RowsTable({ view, group }: { view: UsageView; group: UsageGroup }) {
  const first = { day: "Day", model: "Model", thread: "Session", user: "Person" }[group];
  if (!view.rows.length) return <Empty title="Nothing used in this period" />;
  return (
    <div className="overflow-x-auto rounded-lg border">
      <table className="w-full text-sm" aria-label={`Usage ${GROUPS.find((g) => g.key === group)?.label.toLowerCase()}`}>
        <thead className="bg-muted/40 text-left text-xs text-muted-foreground">
          <tr>
            <th className="px-3 py-2 font-normal">{first}</th>
            <th className="px-3 py-2 text-right font-normal">Runs</th>
            <th className="px-3 py-2 text-right font-normal">Input</th>
            <th className="px-3 py-2 text-right font-normal">Output</th>
            <th className="px-3 py-2 text-right font-normal">Cache read</th>
            <th className="px-3 py-2 text-right font-normal">Cache write</th>
            <th className="px-3 py-2 text-right font-normal">Cost</th>
          </tr>
        </thead>
        <tbody>
          {view.rows.map((r) => (
            <tr key={r.key} className="border-t">
              <td className="max-w-64 truncate px-3 py-2">
                {group === "thread" ? (
                  <Link to="/t/$threadId" params={{ threadId: r.key }} className="text-primary hover:underline">
                    {view.titles?.[r.key] || r.key}
                  </Link>
                ) : (
                  r.key || "(none)"
                )}
              </td>
              <td className="px-3 py-2 text-right tabular-nums">{r.runs}</td>
              <td className="px-3 py-2 text-right tabular-nums">{tokens(r.inputTokens)}</td>
              <td className="px-3 py-2 text-right tabular-nums">{tokens(r.outputTokens)}</td>
              <td className="px-3 py-2 text-right tabular-nums">{tokens(r.cacheReadTokens)}</td>
              <td className="px-3 py-2 text-right tabular-nums">{tokens(r.cacheWriteTokens)}</td>
              <td className="px-3 py-2 text-right tabular-nums">
                {money(r.costUsd)}
                {r.unpriced ? (
                  <span className="ml-1 text-xs text-warning-foreground dark:text-warning" title="No price is known for this model: the cost leaves it out">
                    unpriced
                  </span>
                ) : null}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

const today = () => new Date().toISOString().slice(0, 10);
const daysAgo = (n: number) => new Date(Date.now() - n * 864e5).toISOString().slice(0, 10);

/** The usage of a project (`projectId`), or the caller's own without one. */
export function UsagePanel({ projectId }: { projectId?: string }) {
  const [range, setRange] = useState("month");
  const [from, setFrom] = useState(daysAgo(30));
  const [to, setTo] = useState(today());
  const [group, setGroup] = useState<UsageGroup>("day");
  const effective = range === "custom" ? `${from}..${to}` : range;
  const days = useUsage({ projectId, range: effective, group: "day" });
  const grouped = useUsage({ projectId, range: effective, group });
  const view = group === "day" ? days.data : grouped.data;
  const q = group === "day" ? days : grouped;
  if (q.error && isUnsupported(q.error)) return <Empty title="Usage is not available">This server does not keep a usage ledger.</Empty>;
  const total = view?.total;
  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-end gap-3">
        <Tabs value={range} onValueChange={setRange}>
          <TabsList>
            {RANGES.map((r) => (
              <TabsTrigger key={r.key} value={r.key}>
                {r.label}
              </TabsTrigger>
            ))}
          </TabsList>
        </Tabs>
        {range === "custom" ? (
          <div className="flex items-end gap-2">
            <div className="space-y-1">
              <Label htmlFor="us-from">From</Label>
              <Input id="us-from" type="date" value={from} max={to} onChange={(e) => setFrom(e.target.value)} className="w-40" />
            </div>
            <div className="space-y-1">
              <Label htmlFor="us-to">To</Label>
              <Input id="us-to" type="date" value={to} min={from} onChange={(e) => setTo(e.target.value)} className="w-40" />
            </div>
          </div>
        ) : null}
      </div>
      {q.isLoading ? <ListSkeleton rows={3} /> : q.error || !view || !total ? <ErrorState error={q.error} retry={() => void q.refetch()} /> : (
        <>
          <CapCard cap={view.cap} canSet={view.canSetCap} projectId={projectId} />
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
            <Tile label="Cost" value={money(total.costUsd)} hint={total.unpriced ? `${total.unpriced} unpriced: the real cost is higher` : undefined} />
            <Tile label="Runs" value={total.runs.toLocaleString()} />
            <Tile label="Input / output tokens" value={`${tokens(total.inputTokens)} / ${tokens(total.outputTokens)}`} />
            <Tile label="Cache hit rate" value={`${Math.round(cacheHit(total) * 100)}%`} hint={`${tokens(total.cacheReadTokens)} read, ${tokens(total.cacheWriteTokens)} written`} />
          </div>
          <TokenSplit total={total} />
          <section aria-label="By kind of work" className="rounded-lg border">
            <ul className="divide-y">
              {KINDS.map((k) => {
                const t = view.bySource[k.key];
                return (
                  <li key={k.key} className="flex items-center gap-3 px-3 py-2 text-sm">
                    <span className="flex-1">{k.label}</span>
                    <span className="text-xs text-muted-foreground tabular-nums">{t?.runs ?? 0} runs</span>
                    <span className="w-24 text-right tabular-nums">{money(t?.costUsd ?? 0)}</span>
                  </li>
                );
              })}
            </ul>
          </section>
          {days.data ? <DayChart rows={days.data.rows} /> : null}
          <Tabs value={group} onValueChange={(g) => setGroup(g as UsageGroup)}>
            <TabsList>
              {GROUPS.map((g) => (
                <TabsTrigger key={g.key} value={g.key}>
                  {g.label}
                </TabsTrigger>
              ))}
            </TabsList>
          </Tabs>
          <RowsTable view={view} group={group} />
          <p className="text-xs text-muted-foreground">
            Costs use the model price table at the time the run ended; a model with no known price counts its tokens and is flagged unpriced. Caps count the calendar month in UTC.
          </p>
        </>
      )}
    </div>
  );
}

/** The personal usage, at /usage. */
export function UsagePage() {
  return (
    <ScrollArea className="h-full">
      <div className="mx-auto w-full max-w-5xl space-y-4 p-4 md:p-8">
        <h1 className="text-xl font-semibold">Usage</h1>
        <p className="text-sm text-muted-foreground">What your sessions, your factory tasks and your scheduled tasks consumed, in every project and on your own.</p>
        <UsagePanel />
      </div>
    </ScrollArea>
  );
}
