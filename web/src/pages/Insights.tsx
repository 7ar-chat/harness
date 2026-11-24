// Insights: a few plain answers about what the agents delivered and what it cost. One headline row, four small charts, and the tasks
// that need a look. The same numbers can be downloaded as a self-contained report (the server renders it).
import { Link } from "@tanstack/react-router";
import { Download, Link2 } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";
import { INSIGHT_RANGES, reportUrl, useInsights, type InsightsView } from "@/api/insights";
import { Empty, ErrorState, ListSkeleton } from "@/components/app/common";
import { DigestCard } from "./DigestCard";
import { Button } from "@/components/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { cn } from "@/lib/utils";

export const money = (usd: number) => (usd >= 100 ? `$${usd.toFixed(0)}` : usd >= 0.01 || usd === 0 ? `$${usd.toFixed(2)}` : `$${usd.toFixed(4)}`);

/** Seconds as "3h 10m", "2d 4h", "45s". */
export function duration(sec: number): string {
  const s = Math.round(sec);
  if (s < 1) return "0s";
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) {
    const h = Math.floor(s / 3600);
    const m = Math.floor((s % 3600) / 60);
    return m ? `${h}h ${m}m` : `${h}h`;
  }
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  return h ? `${d}d ${h}h` : `${d}d`;
}

const pct = (r: number) => `${Math.round(r * 100)}%`;

/** Chart colours: the fixed categorical order, light and dark steps (never cycled: the sixth is "Other"). */
const SERIES = ["var(--viz-1)", "var(--viz-2)", "var(--viz-3)", "var(--viz-4)", "var(--viz-5)", "var(--viz-other)"];
const VIZ_VARS =
  "[--viz-1:#2a78d6] [--viz-2:#eb6834] [--viz-3:#1baf7a] [--viz-4:#eda100] [--viz-5:#e87ba4] [--viz-other:#6e6d68] " +
  "dark:[--viz-1:#3987e5] dark:[--viz-2:#d95926] dark:[--viz-3:#199e70] dark:[--viz-4:#c98500] dark:[--viz-5:#d55181] dark:[--viz-other:#9a998f]";

function Tile({ label, value, hint }: { label: string; value: string; hint: string }) {
  return (
    <div className="rounded-lg border p-3" title={hint}>
      <div className="text-xs font-medium text-muted-foreground">{label}</div>
      <div className="mt-1 text-2xl font-semibold tabular-nums">{value}</div>
      <div className="mt-0.5 text-xs text-muted-foreground">{hint}</div>
    </div>
  );
}

function niceMax(v: number) {
  if (v <= 0) return 1;
  const e = 10 ** Math.floor(Math.log10(v));
  return ([1, 2, 2.5, 5, 10].find((m) => v <= m * e) ?? 10) * e;
}

const W = 640;
const H = 190;
const pad = { l: 40, r: 8, t: 10, b: 26 };

/** Gridlines at 0, half and the top, with their labels. */
function Axes({ max, fmt }: { max: number; fmt: (v: number) => string }) {
  return (
    <>
      {[0, 0.5, 1].map((f) => {
        const y = H - pad.b - f * (H - pad.t - pad.b);
        return (
          <g key={f}>
            <line x1={pad.l} x2={W - pad.r} y1={y} y2={y} className={f === 0 ? "stroke-muted-foreground/60" : "stroke-border"} />
            <text x={pad.l - 6} y={y + 3.5} textAnchor="end" className="fill-muted-foreground text-[10px]">
              {fmt(max * f)}
            </text>
          </g>
        );
      })}
    </>
  );
}

/** X labels, thinned so they never touch. */
function XLabels({ labels, slot }: { labels: string[]; slot: number }) {
  const step = Math.ceil(labels.length / 8) || 1;
  return (
    <>
      {labels.map((l, i) =>
        i % step ? null : (
          <text key={i} x={pad.l + slot * (i + 0.5)} y={H - 8} textAnchor="middle" className="fill-muted-foreground text-[10px]">
            {l}
          </text>
        ),
      )}
    </>
  );
}

function Chart({ title, note, children, legend }: { title: string; note?: string; children: React.ReactNode; legend?: { color: string; label: string }[] }) {
  return (
    <figure className="rounded-lg border p-3">
      <figcaption className="mb-1">
        <div className="text-sm font-medium">{title}</div>
        {note ? <div className="text-xs text-muted-foreground">{note}</div> : null}
      </figcaption>
      {children}
      {legend?.length ? (
        <ul className="mt-1 flex flex-wrap gap-x-4 gap-y-1 text-xs">
          {legend.map((l) => (
            <li key={l.label} className="flex items-center gap-1.5">
              <span className="size-2.5 rounded-sm" style={{ background: l.color }} aria-hidden />
              <span className="text-muted-foreground">{l.label}</span>
            </li>
          ))}
        </ul>
      ) : null}
    </figure>
  );
}

function Bars({ labels, values, label, fmt = (v: number) => String(v) }: { labels: string[]; values: number[]; label: string; fmt?: (v: number) => string }) {
  const top = Math.max(4, ...values);
  const max = top <= 20 ? Math.ceil(top / 2) * 2 : niceMax(top); // counts: even tops, so the middle tick is a whole number
  const slot = (W - pad.l - pad.r) / Math.max(values.length, 1);
  const bw = Math.min(slot * 0.62, 44);
  return (
    <svg viewBox={`0 0 ${W} ${H}`} role="img" aria-label={label} className="h-auto w-full">
      <Axes max={max} fmt={fmt} />
      {values.map((v, i) => {
        const h = (v / max) * (H - pad.t - pad.b);
        const x = pad.l + slot * i + (slot - bw) / 2;
        return v > 0 ? (
          <g key={i}>
            <rect x={x} y={H - pad.b - h} width={bw} height={h} rx={3} fill={SERIES[0]}>
              <title>{`${labels[i]}: ${fmt(v)}`}</title>
            </rect>
            <text x={x + bw / 2} y={H - pad.b - h - 4} textAnchor="middle" className="fill-foreground text-[10px] font-medium">
              {fmt(v)}
            </text>
          </g>
        ) : null;
      })}
      <XLabels labels={labels} slot={slot} />
    </svg>
  );
}

const shortDay = (d: string) => {
  const t = new Date(`${d}T12:00:00`);
  return Number.isNaN(t.getTime()) ? d : t.toLocaleDateString(undefined, { day: "numeric", month: "short" });
};

function CostByDay({ cost }: { cost: InsightsView["cost"] }) {
  const models = cost.models.map((m) => m.model);
  const idx = (m: string) => Math.min(models.indexOf(m), 5);
  const days = cost.byDay;
  const max = niceMax(Math.max(0, ...days.map((d) => d.usd)));
  const slot = (W - pad.l - pad.r) / Math.max(days.length, 1);
  const bw = Math.max(Math.min(slot * 0.7, 28), 1.5);
  const legend = models.slice(0, 5).map((m, i) => ({ color: SERIES[i]!, label: m }));
  if (models.length > 5) legend.push({ color: SERIES[5]!, label: "Other" });
  return (
    <Chart title="Cost per day" note={cost.unpricedRuns ? `${cost.unpricedRuns} run(s) used a model without a price: not in these dollars.` : "Split by model."} legend={legend}>
      <svg viewBox={`0 0 ${W} ${H}`} role="img" aria-label={`Cost per day, ${days.length} days, total ${money(cost.totalUsd)}`} className="h-auto w-full">
        <Axes max={max} fmt={money} />
        {days.map((d, i) => {
          let y = H - pad.b;
          const x = pad.l + slot * i + (slot - bw) / 2;
          const parts = Object.entries(d.byModel ?? {}).sort((a, b) => idx(a[0]) - idx(b[0]) || a[0].localeCompare(b[0]));
          return (
            <g key={d.day}>
              {parts.map(([m, v]) => {
                if (v <= 0) return null;
                const h = Math.max((v / max) * (H - pad.t - pad.b), 0.5);
                y -= h;
                return (
                  <rect key={m} x={x} y={y} width={bw} height={h} fill={SERIES[idx(m)]} className="stroke-background" strokeWidth={1}>
                    <title>{`${d.day}, ${m}: ${money(v)}`}</title>
                  </rect>
                );
              })}
            </g>
          );
        })}
        <XLabels labels={days.map((d) => shortDay(d.day))} slot={slot} />
      </svg>
    </Chart>
  );
}

function TimeSplit({ d }: { d: InsightsView["delivery"] }) {
  const total = d.workingSec + d.waitingSec;
  const w = total > 0 ? (d.workingSec / total) * (W - 2) : 0;
  return (
    <Chart
      title="Where the time goes"
      note="Across merged PRs: agents working versus waiting for a person."
      legend={
        total > 0
          ? [
              { color: SERIES[0]!, label: `Agents working ${duration(d.workingSec)}` },
              { color: SERIES[1]!, label: `Waiting on a person ${duration(d.waitingSec)}` },
            ]
          : []
      }
    >
      {total > 0 ? (
        <svg viewBox={`0 0 ${W} 48`} role="img" aria-label={`Working ${pct(d.workingSec / total)}, waiting on a person ${pct(d.waitingSec / total)}`} className="h-auto w-full">
          <rect x={0} y={6} width={w} height={32} rx={4} fill={SERIES[0]}>
            <title>{`Agents working: ${duration(d.workingSec)}`}</title>
          </rect>
          <rect x={w + 2} y={6} width={Math.max(W - 2 - w, 0)} height={32} rx={4} fill={SERIES[1]}>
            <title>{`Waiting on a person: ${duration(d.waitingSec)}`}</title>
          </rect>
          <text x={8} y={27} className="fill-white text-[12px] font-semibold">
            {pct(d.workingSec / total)}
          </text>
          <text x={W - 8} y={27} textAnchor="end" className="fill-white text-[12px] font-semibold">
            {pct(d.waitingSec / total)}
          </text>
        </svg>
      ) : (
        <p className="py-4 text-sm text-muted-foreground">No merged PRs in this period yet.</p>
      )}
    </Chart>
  );
}

const WAIT_KINDS = [
  ["tools", "Tool approvals", "A risky tool call that waited in a session."],
  ["tasks", "Task escalations", "A task blocked until a person decided."],
  ["gates", "Workflow gates", "A workflow step that waited for a person."],
] as const;

/** The headline "waiting on a person", split by what was waited for. Tool approvals are only known from when they started being recorded. */
function WaitBreakdown({ w }: { w?: InsightsView["waiting"] }) {
  if (!w) return null;
  const known = WAIT_KINDS.some(([k]) => w[k].n > 0);
  return (
    <section className="rounded-lg border" aria-label="Time waiting on a person">
      <h3 className="p-3 pb-1 text-sm font-medium">Time waiting on a person</h3>
      {known ? (
        <table className="w-full text-sm">
          <thead className="text-xs text-muted-foreground">
            <tr>
              <th className="p-2 pl-3 text-left font-medium">What was waited for</th>
              <th className="p-2 text-right font-medium">Decided</th>
              <th className="p-2 text-right font-medium">Median wait</th>
              <th className="p-2 pr-3 text-right font-medium">Total</th>
            </tr>
          </thead>
          <tbody>
            {WAIT_KINDS.map(([k, label, hint]) => (
              <tr key={k} className="border-t" title={hint}>
                <td className="p-2 pl-3">{label}</td>
                <td className="p-2 text-right tabular-nums">{w[k].n || "n/a"}</td>
                <td className="p-2 text-right tabular-nums">{w[k].n ? duration(w[k].medianSec) : "n/a"}</td>
                <td className="p-2 pr-3 text-right tabular-nums">{w[k].n ? duration(w[k].totalSec) : "n/a"}</td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : (
        <p className="p-3 pt-1 text-sm text-muted-foreground">Nothing was decided by a person in this period, or it was decided before waits were recorded.</p>
      )}
    </section>
  );
}

function People({ people }: { people: InsightsView["people"] }) {
  if (!people.length) return null;
  return (
    <section className="rounded-lg border" aria-label="People">
      <h3 className="p-3 pb-1 text-sm font-medium">People</h3>
      <div className="overflow-x-auto">
        <table className="w-full text-sm">
          <thead className="text-xs text-muted-foreground">
            <tr>
              <th className="p-2 pl-3 text-left font-medium">Member</th>
              <th className="p-2 text-right font-medium">Sessions</th>
              <th className="p-2 text-right font-medium">Runs</th>
              <th className="p-2 text-right font-medium">Approvals decided</th>
              <th className="p-2 text-right font-medium" title="Median time between a request and this person's decision (tool approvals, task escalations, workflow gates)">
                Median wait
              </th>
              <th className="p-2 pr-3 text-right font-medium">Cost</th>
            </tr>
          </thead>
          <tbody>
            {people.map((p) => {
              const l = p.latency;
              const detail = l?.n ? WAIT_KINDS.filter(([k]) => p.waits[k].n > 0).map(([k, label]) => `${label}: ${duration(p.waits[k].medianSec)} (${p.waits[k].n})`).join("; ") : "";
              return (
                <tr key={p.login} className="border-t">
                  <td className="p-2 pl-3">{p.login}</td>
                  <td className="p-2 text-right tabular-nums">{p.sessions}</td>
                  <td className="p-2 text-right tabular-nums">{p.runs}</td>
                  <td className="p-2 text-right tabular-nums">{p.approvals}</td>
                  <td className="p-2 text-right tabular-nums" title={detail || "Not recorded for these decisions"}>
                    {l?.n ? duration(l.medianSec) : "n/a"}
                  </td>
                  <td className="p-2 pr-3 text-right tabular-nums">{money(p.costUsd)}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </section>
  );
}

type Ref = InsightsView["attention"]["slowest"][number];

function Attention({ title, items, fmt, empty, projectId }: { title: string; items: Ref[]; fmt: (v: number) => string; empty: string; projectId?: string }) {
  return (
    <section className="rounded-lg border p-3">
      <h3 className="text-sm font-medium">{title}</h3>
      {items.length ? (
        <ol className="mt-2 space-y-1.5 text-sm">
          {items.map((t) => (
            <li key={t.id} className="flex items-baseline gap-2">
              {projectId ? (
                <Link to="/p/$projectId" params={{ projectId }} search={{ tab: "tasks", task: t.id }} className="min-w-0 flex-1 truncate text-primary hover:underline">
                  {t.title || t.id}
                </Link>
              ) : (
                <span className="min-w-0 flex-1 truncate">{t.title || t.id}</span>
              )}
              <span className="shrink-0 tabular-nums text-muted-foreground" title={t.detail}>
                {fmt(t.value)}
              </span>
            </li>
          ))}
        </ol>
      ) : (
        <p className="mt-2 text-sm text-muted-foreground">{empty}</p>
      )}
    </section>
  );
}

/** The report menu: HTML opens in a new page (it is a static document), CSV and JSON download. */
function ReportMenu({ projectId, range }: { projectId: string; range: string }) {
  const copy = async () => {
    const url = new URL(reportUrl(projectId, range, "html"), window.location.origin).toString();
    try {
      await navigator.clipboard.writeText(url);
      toast.success("Link copied. Whoever opens it must be a member of this project.");
    } catch {
      toast.error("Could not copy the link.");
    }
  };
  return (
    <div className="flex items-center gap-2">
      <Button variant="ghost" size="sm" onClick={() => void copy()}>
        <Link2 className="size-4" /> Copy link
      </Button>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button variant="outline" size="sm">
            <Download className="size-4" /> Download report
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuItem asChild>
            <a href={reportUrl(projectId, range, "html")} target="_blank" rel="noreferrer">
              Report page (HTML)
            </a>
          </DropdownMenuItem>
          <DropdownMenuItem asChild>
            <a href={reportUrl(projectId, range, "html", true)}>Save report (HTML file)</a>
          </DropdownMenuItem>
          <DropdownMenuItem asChild>
            <a href={reportUrl(projectId, range, "csv", true)}>Task table (CSV)</a>
          </DropdownMenuItem>
          <DropdownMenuItem asChild>
            <a href={reportUrl(projectId, range, "json", true)}>Raw numbers (JSON)</a>
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
    </div>
  );
}

function Body({ d, projectId }: { d: InsightsView; projectId?: string }) {
  const dl = d.delivery;
  const c = d.cost;
  const merged = dl.merged;
  return (
    <div className="space-y-4">
      <p className="rounded-md border-l-4 border-primary bg-muted/50 p-3 text-sm" data-testid="insights-summary">
        {d.summary}
      </p>
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <Tile label="Merged PRs" value={String(merged)} hint="Pull requests the agents delivered and that were merged." />
        <Tile label="First-pass success" value={merged ? pct(dl.firstPassRate) : "n/a"} hint="Merged with no failed CI, no review round and one attempt." />
        <Tile label="Cost per PR" value={merged ? money(c.perMergedUsd) : "n/a"} hint="All model spend divided by merged PRs." />
        <Tile label="Time to merge" value={merged ? duration(dl.cycleMedianSec) : "n/a"} hint="Median, from a task's first run to its merge." />
      </div>
      <div className={cn("grid gap-3 lg:grid-cols-2", VIZ_VARS)}>
        <Chart title="Merged PRs per week" note="Weeks start on Monday.">
          <Bars labels={d.flow.weeks.map((w) => shortDay(w.week))} values={d.flow.weeks.map((w) => w.merged)} label={`Merged PRs per week, ${d.flow.weeks.length} weeks, ${merged} in total`} />
        </Chart>
        <CostByDay cost={c} />
        <Chart title="Time to merge" note="How many merged PRs took how long.">
          <Bars labels={dl.cycleBuckets.map((b) => b.label)} values={dl.cycleBuckets.map((b) => b.count)} label="Distribution of time to merge" />
        </Chart>
        <TimeSplit d={dl} />
      </div>
      <WaitBreakdown w={d.waiting} />
      <h2 className="pt-2 text-sm font-semibold">What needs attention</h2>
      <div className="grid gap-3 sm:grid-cols-2">
        <Attention title="Slowest tasks" items={d.attention.slowest} fmt={duration} empty="Nothing finished or running yet." projectId={projectId} />
        <Attention title="Blocked the longest" items={d.attention.blockedLongest} fmt={duration} empty="No task is waiting for a person." projectId={projectId} />
        <Attention title="Most expensive" items={d.attention.mostExpensive} fmt={money} empty="No priced spend yet." projectId={projectId} />
        <Attention title="Most reworked" items={d.attention.mostReworked} fmt={(v) => `${v} round${v === 1 ? "" : "s"}`} empty="No task needed rework." projectId={projectId} />
      </div>
      <People people={d.people} />
      {c.models.length ? (
        <section className="rounded-lg border">
          <h3 className="p-3 pb-1 text-sm font-medium">Cost by model</h3>
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead className="text-xs text-muted-foreground">
                <tr>
                  <th className="p-2 pl-3 text-left font-medium">Model</th>
                  <th className="p-2 text-right font-medium">Cache read</th>
                  <th className="p-2 text-right font-medium">Cache write</th>
                  <th className="p-2 pr-3 text-right font-medium">Cost</th>
                </tr>
              </thead>
              <tbody>
                {c.models.map((m) => (
                  <tr key={m.model} className="border-t">
                    <td className="p-2 pl-3">
                      {m.model}
                      {m.unpriced ? <span className="ml-2 text-xs text-warning">no price</span> : null}
                    </td>
                    <td className="p-2 text-right tabular-nums">{m.cacheReadTokens.toLocaleString()}</td>
                    <td className="p-2 text-right tabular-nums">{m.cacheWriteTokens.toLocaleString()}</td>
                    <td className="p-2 pr-3 text-right tabular-nums">{money(m.costUsd)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      ) : null}
      <p className="text-xs text-muted-foreground">
        Waiting on a person is the time a task spent blocked: an approval, a question, or ready to merge. Days are cut in {d.tz}.
      </p>
    </div>
  );
}

function RangePicker({ value, onChange }: { value: string; onChange: (r: string) => void }) {
  return (
    <div role="group" aria-label="Period" className="inline-flex rounded-md border p-0.5">
      {INSIGHT_RANGES.map((r) => (
        <button
          key={r.key}
          type="button"
          aria-pressed={value === r.key}
          onClick={() => onChange(r.key)}
          className={cn("rounded px-2.5 py-1 text-xs font-medium focus-visible:ring-2 focus-visible:ring-ring", value === r.key ? "bg-primary text-primary-foreground" : "text-muted-foreground hover:text-foreground")}
        >
          {r.key.replace("d", " days")}
        </button>
      ))}
    </div>
  );
}

/** A project's Insights tab. */
export function InsightsPanel({ projectId }: { projectId: string }) {
  const [range, setRange] = useState("30d");
  const q = useInsights(projectId, range);
  return (
    <div className="space-y-4" data-testid="insights">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <RangePicker value={range} onChange={setRange} />
        <ReportMenu projectId={projectId} range={range} />
      </div>
      <DigestCard projectId={projectId} />
      {q.isLoading ? (
        <ListSkeleton rows={3} />
      ) : q.error || !q.data ? (
        <ErrorState error={q.error} retry={() => void q.refetch()} />
      ) : q.data.empty ? (
        <Empty title="Insights appear after the first merged task">
          Nothing was delivered in this period yet.{" "}
          <Link to="/p/$projectId" params={{ projectId }} search={{ tab: "tasks" }} className="text-primary underline underline-offset-2">
            Hand off a first task
          </Link>{" "}
          and come back: you will see what it cost and how long it took.
        </Empty>
      ) : (
        <Body d={q.data} projectId={projectId} />
      )}
    </div>
  );
}

/** The personal page: the same answers for the caller's own work across projects. */
export function InsightsPage() {
  const [range, setRange] = useState("30d");
  const q = useInsights(undefined, range);
  return (
    <div className="mx-auto w-full max-w-5xl space-y-4 p-4 sm:p-6" data-testid="insights">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h1 className="text-lg font-semibold">My insights</h1>
          <p className="text-sm text-muted-foreground">What your own tasks and runs delivered and cost, across projects.</p>
        </div>
        <RangePicker value={range} onChange={setRange} />
      </div>
      {q.isLoading ? (
        <ListSkeleton rows={3} />
      ) : q.error || !q.data ? (
        <ErrorState error={q.error} retry={() => void q.refetch()} />
      ) : q.data.empty ? (
        <Empty title="Insights appear after your first merged task">Nothing of yours was delivered in this period yet.</Empty>
      ) : (
        <Body d={q.data} />
      )}
    </div>
  );
}
