import { formatDistanceToNowStrict } from "date-fns";
import { Check, ExternalLink, Plus, RefreshCw, X } from "lucide-react";
import { useEffect, useState } from "react";
import { isUnsupported } from "@/api/client";
import { useAddJobBoard, useJobs, useSaveJobProfile, useScanJobs, useSetJobStatus, type Job, type JobProfile } from "@/api/queries";
import { Empty, ErrorState, ListSkeleton } from "@/components/app/common";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";

const lines = (s: string) => s.split(/[\n,]/).map((x) => x.trim()).filter(Boolean);
const text = (a?: string[]) => (a ?? []).join(", ");

/** The groups of the list: what needs the person first. */
const GROUPS: { key: string; label: string; statuses: string[]; empty: string }[] = [
  { key: "queued", label: "Needs you", statuses: ["queued", "failed"], empty: "Nothing waits for you." },
  { key: "new", label: "New", statuses: ["new"], empty: "No new jobs. The search looks again every hour." },
  { key: "approved", label: "Approved", statuses: ["approved", "applying"], empty: "Nothing approved yet." },
  { key: "applied", label: "Applied", statuses: ["applied", "interview", "rejected", "offer", "withdrawn"], empty: "No applications yet." },
];

function JobRow({ job }: { job: Job }) {
  const set = useSetJobStatus();
  const act = (status: string) => set.mutate({ id: job.id, status });
  const open = job.status === "queued" || job.status === "new" || job.status === "failed";
  return (
    <li className="flex flex-wrap items-start gap-3 border-b px-3 py-3 last:border-0">
      <div className="min-w-0 flex-1 space-y-1">
        <div className="flex flex-wrap items-center gap-2">
          <a href={job.url} target="_blank" rel="noreferrer noopener" className="inline-flex items-center gap-1 font-medium hover:underline">
            {job.title}
            <ExternalLink className="size-3 text-muted-foreground" aria-hidden />
          </a>
          {job.score ? <Badge variant="secondary">{job.score.toFixed(1)}</Badge> : null}
          {job.status !== "queued" && job.status !== "new" ? <Badge variant="outline">{job.status}</Badge> : null}
        </div>
        <p className="text-sm text-muted-foreground">
          {job.company}
          {job.location ? ` · ${job.location}` : ""}
          {job.remote ? " · remote" : ""} · found {formatDistanceToNowStrict(new Date(job.seen))} ago
        </p>
        {job.reason ? <p className="text-sm">{job.reason}</p> : null}
        {job.note ? <p className="text-sm text-muted-foreground">{job.note}</p> : null}
      </div>
      <div className="flex shrink-0 gap-2">
        {open ? (
          <>
            <Button size="sm" onClick={() => act("approved")} disabled={set.isPending}>
              <Check /> Apply
            </Button>
            <Button size="sm" variant="outline" onClick={() => act("skipped")} disabled={set.isPending}>
              <X /> Skip
            </Button>
          </>
        ) : null}
        {job.status === "approved" ? (
          <Button size="sm" variant="outline" onClick={() => act("queued")} disabled={set.isPending}>
            Hold
          </Button>
        ) : null}
        {job.status === "applied" ? (
          <>
            <Button size="sm" variant="outline" onClick={() => act("interview")} disabled={set.isPending}>
              Interview
            </Button>
            <Button size="sm" variant="ghost" onClick={() => act("rejected")} disabled={set.isPending}>
              Rejected
            </Button>
          </>
        ) : null}
      </div>
    </li>
  );
}

function ProfileCard({ profile }: { profile: JobProfile }) {
  const save = useSaveJobProfile();
  const addBoard = useAddJobBoard();
  const scan = useScanJobs();
  const [f, setF] = useState(() => draft(profile));
  const [board, setBoard] = useState("");
  useEffect(() => setF(draft(profile)), [profile]);
  const dirty = JSON.stringify(f) !== JSON.stringify(draft(profile));
  const submit = () =>
    save.mutate({
      ...profile,
      enabled: f.enabled,
      mode: f.auto ? "auto" : "queue",
      titles: lines(f.titles),
      exclude: lines(f.exclude),
      locations: lines(f.locations),
      remoteOnly: f.remoteOnly,
      minSalary: Number(f.minSalary) || 0,
      currency: f.currency,
      needsSponsorship: f.sponsor,
      blacklist: lines(f.blacklist),
      dealbreakers: f.dealbreakers,
      dailyCap: Number(f.dailyCap) || 10,
      minScore: Number(f.minScore) || 4.5,
    });
  const field = (k: keyof typeof f, label: string, hint?: string, area = false) => (
    <div className="space-y-1.5">
      <Label htmlFor={`jp-${k}`}>{label}</Label>
      {area ? (
        <Textarea id={`jp-${k}`} rows={2} value={String(f[k])} onChange={(e) => setF({ ...f, [k]: e.target.value })} />
      ) : (
        <Input id={`jp-${k}`} value={String(f[k])} onChange={(e) => setF({ ...f, [k]: e.target.value })} />
      )}
      {hint ? <p className="text-xs text-muted-foreground">{hint}</p> : null}
    </div>
  );
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Your search</CardTitle>
        <CardDescription>Your assistant searches and judges by itself. It applies only within these limits.</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="flex items-center justify-between gap-4">
          <div>
            <Label htmlFor="jp-enabled">Search around the clock</Label>
            <p className="text-xs text-muted-foreground">Scans your companies every hour and wakes your assistant when there is something to do.</p>
          </div>
          <Switch id="jp-enabled" checked={f.enabled} onCheckedChange={(v) => setF({ ...f, enabled: v })} />
        </div>
        <div className="flex items-center justify-between gap-4">
          <div>
            <Label htmlFor="jp-auto">Apply without asking</Label>
            <p className="text-xs text-muted-foreground">Off: it prepares and waits for you to press Apply. On: it submits jobs scored {f.minScore} or more.</p>
          </div>
          <Switch id="jp-auto" checked={f.auto} onCheckedChange={(v) => setF({ ...f, auto: v })} />
        </div>
        <div className="grid gap-4 md:grid-cols-2">
          {field("titles", "Roles", "Comma separated: a job matches when its title contains one.")}
          {field("exclude", "Never these in a title", "intern, junior, manager")}
          {field("locations", "Places", "Paris, France, remote. Empty: anywhere.")}
          {field("blacklist", "Never apply to", "Company names or domains.")}
          {field("minSalary", "Salary floor (yearly)")}
          {field("currency", "Currency")}
          {field("minScore", "Auto-apply from score", "1 to 5. Be strict: 4.5.")}
          {field("dailyCap", "Applications per day, at most")}
        </div>
        <div className="flex flex-wrap items-center gap-6">
          <label className="flex items-center gap-2 text-sm">
            <Switch checked={f.remoteOnly} onCheckedChange={(v) => setF({ ...f, remoteOnly: v })} /> Remote only
          </label>
          <label className="flex items-center gap-2 text-sm">
            <Switch checked={f.sponsor} onCheckedChange={(v) => setF({ ...f, sponsor: v })} /> I need visa sponsorship
          </label>
        </div>
        {field("dealbreakers", "Dealbreakers", "Anything that rules a job out, in your words.", true)}
        <div className="flex justify-end">
          <Button onClick={submit} disabled={!dirty || save.isPending}>
            Save
          </Button>
        </div>
        <div className="space-y-2 border-t pt-4">
          <Label htmlFor="jp-board">Companies to watch ({profile.companies?.length ?? 0})</Label>
          <p className="text-xs text-muted-foreground">Paste a job-board link (jobs.lever.co/…, boards.greenhouse.io/…, jobs.ashbyhq.com/…). Your assistant can also find them for you.</p>
          <div className="flex gap-2">
            <Input id="jp-board" value={board} placeholder="https://jobs.lever.co/acme" onChange={(e) => setBoard(e.target.value)} />
            <Button variant="outline" disabled={!board.trim() || addBoard.isPending} onClick={() => addBoard.mutate(board.trim(), { onSuccess: () => setBoard("") })}>
              <Plus /> Add
            </Button>
            <Button variant="outline" disabled={scan.isPending} onClick={() => scan.mutate()}>
              <RefreshCw className={scan.isPending ? "animate-spin" : ""} /> Scan now
            </Button>
          </div>
          {profile.companies?.length ? (
            <div className="flex flex-wrap gap-1.5 pt-1">
              {profile.companies.map((c) => (
                <Badge key={`${c.ats}/${c.slug}`} variant="secondary">
                  {c.name}
                </Badge>
              ))}
            </div>
          ) : null}
        </div>
      </CardContent>
    </Card>
  );
}

function draft(p: JobProfile) {
  return {
    enabled: p.enabled,
    auto: p.mode === "auto",
    titles: text(p.titles),
    exclude: text(p.exclude),
    locations: text(p.locations),
    blacklist: text(p.blacklist),
    minSalary: p.minSalary ? String(p.minSalary) : "",
    currency: p.currency ?? "",
    minScore: String(p.minScore),
    dailyCap: String(p.dailyCap),
    remoteOnly: !!p.remoteOnly,
    sponsor: !!p.needsSponsorship,
    dealbreakers: p.dealbreakers ?? "",
  };
}

export function JobsPage() {
  const q = useJobs();
  const [group, setGroup] = useState("queued");
  const shell = (children: React.ReactNode) => (
    <ScrollArea className="h-full">
      <div className="mx-auto w-full max-w-5xl space-y-4 p-4 md:p-8">
        <header className="space-y-1">
          <h1 className="text-xl font-semibold">Jobs</h1>
          <p className="text-sm text-muted-foreground">Your assistant looks for roles that fit you and applies for you. You decide how much freedom it has.</p>
        </header>
        {children}
      </div>
    </ScrollArea>
  );
  if (q.error && isUnsupported(q.error)) return shell(<p className="text-sm text-muted-foreground">This server has no job search.</p>);
  if (q.error) return shell(<ErrorState error={q.error} />);
  if (!q.data) return shell(<ListSkeleton />);
  const { profile, counts, jobs } = q.data;
  const g = GROUPS.find((x) => x.key === group) ?? GROUPS[0]!;
  const shown = jobs.filter((j) => g.statuses.includes(j.status));
  return shell(
    <>
      <ProfileCard profile={profile} />
      <Tabs value={group} onValueChange={setGroup}>
        <TabsList>
          {GROUPS.map((x) => (
            <TabsTrigger key={x.key} value={x.key}>
              {x.label} ({x.statuses.reduce((n, s) => n + (counts[s] ?? 0), 0)})
            </TabsTrigger>
          ))}
        </TabsList>
      </Tabs>
      {shown.length ? (
        <ul className="rounded-lg border" aria-label={g.label}>
          {shown.map((j) => (
            <JobRow key={j.id} job={j} />
          ))}
        </ul>
      ) : (
        <Empty title={g.empty} />
      )}
    </>,
  );
}
