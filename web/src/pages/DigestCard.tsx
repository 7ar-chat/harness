// The weekly digest card of the Insights tab: when the project's report goes out, to whom (the Notifications page),
// "send one now", and the digests already sent, each with a link to its stored report.
import { Link } from "@tanstack/react-router";
import { Download, Send } from "lucide-react";
import { useEffect, useState } from "react";
import { useDigest, usePutDigest, useSendDigest, type DigestView } from "@/api/digest";
import { localZone } from "@/api/insights";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";

const WEEKDAYS = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];
const SELECT = "h-9 rounded-md border border-input bg-background px-2 text-sm shadow-xs focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring";

const when = (iso: string, tz?: string) => {
  try {
    return new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short", timeZone: tz }).format(new Date(iso));
  } catch {
    return iso;
  }
};

const zones = (): string[] => {
  try {
    return (Intl as unknown as { supportedValuesOf?: (k: string) => string[] }).supportedValuesOf?.("timeZone") ?? [];
  } catch {
    return [];
  }
};

function Form({ projectId, v }: { projectId: string; v: DigestView }) {
  const put = usePutDigest(projectId);
  const [enabled, setEnabled] = useState(v.setting.enabled);
  const [weekday, setWeekday] = useState(v.setting.weekday);
  const [hour, setHour] = useState(v.setting.hour);
  const [tz, setTz] = useState(v.setting.timezone === "UTC" && !v.setting.enabled ? localZone() : v.setting.timezone);
  useEffect(() => {
    setEnabled(v.setting.enabled);
    setWeekday(v.setting.weekday);
    setHour(v.setting.hour);
    if (v.setting.enabled || v.setting.timezone !== "UTC") setTz(v.setting.timezone);
  }, [v.setting.enabled, v.setting.weekday, v.setting.hour, v.setting.timezone]);
  const dirty = enabled !== v.setting.enabled || (enabled && (weekday !== v.setting.weekday || hour !== v.setting.hour || tz !== v.setting.timezone));
  return (
    <form
      className="space-y-3"
      onSubmit={(e) => {
        e.preventDefault();
        put.mutate({ enabled, weekday, hour, timezone: tz.trim() || "UTC" });
      }}
    >
      <div className="flex items-center gap-2">
        <Switch id="digest-on" checked={enabled} onCheckedChange={setEnabled} aria-label="Send the weekly digest" />
        <Label htmlFor="digest-on">Send the weekly digest</Label>
      </div>
      <div className={enabled ? "flex flex-wrap items-end gap-3" : "hidden"}>
        <div className="space-y-1">
          <Label htmlFor="digest-day" className="text-xs">Day</Label>
          <select id="digest-day" className={SELECT} value={weekday} onChange={(e) => setWeekday(Number(e.target.value))}>
            {WEEKDAYS.map((d, i) => (
              <option key={d} value={i}>{d}</option>
            ))}
          </select>
        </div>
        <div className="space-y-1">
          <Label htmlFor="digest-hour" className="text-xs">Time</Label>
          <select id="digest-hour" className={SELECT} value={hour} onChange={(e) => setHour(Number(e.target.value))}>
            {Array.from({ length: 24 }, (_, h) => (
              <option key={h} value={h}>{String(h).padStart(2, "0")}:00</option>
            ))}
          </select>
        </div>
        <div className="space-y-1">
          <Label htmlFor="digest-tz" className="text-xs">Time zone</Label>
          <Input id="digest-tz" list="digest-zones" value={tz} onChange={(e) => setTz(e.target.value)} className="w-52" />
          <datalist id="digest-zones">
            {zones().map((z) => (
              <option key={z} value={z} />
            ))}
          </datalist>
        </div>
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <Button type="submit" size="sm" disabled={!dirty || put.isPending}>Save</Button>
        {v.setting.enabled && v.setting.nextAt ? <span className="text-xs text-muted-foreground">Next one: {when(v.setting.nextAt, v.setting.timezone)} ({v.setting.timezone})</span> : null}
      </div>
    </form>
  );
}

/** The card. Hidden when the server keeps no digests. */
export function DigestCard({ projectId }: { projectId: string }) {
  const q = useDigest(projectId);
  const [quiet, setQuiet] = useState<string>();
  const send = useSendDigest(projectId, (m) => setQuiet(m));
  if (q.isLoading || q.error || !q.data) return null;
  const v = q.data;
  return (
    <section aria-labelledby="digest-title" data-testid="digest-card" className="space-y-3 rounded-lg border p-4">
      <div className="flex flex-wrap items-start justify-between gap-2">
        <div>
          <h2 id="digest-title" className="text-sm font-semibold">Weekly digest</h2>
          <p className="text-xs text-muted-foreground">
            Once a week the report of the last seven days is stored here and announced in the app
            {v.notified ? " and on the channels chosen for it" : ""}. A week with nothing in it is skipped. Pick channels under{" "}
            <Link to="/p/$projectId" params={{ projectId }} search={{ tab: "notifications" }} className="text-primary underline underline-offset-2">Notifications</Link>
            , event "The weekly digest is ready".
          </p>
        </div>
        {v.canEdit ? (
          <Button variant="outline" size="sm" disabled={send.isPending} onClick={() => send.mutate({})}>
            <Send className="size-4" aria-hidden /> Send one now
          </Button>
        ) : null}
      </div>
      {v.canEdit ? (
        <Form projectId={projectId} v={v} />
      ) : (
        <p className="text-sm text-muted-foreground">
          {v.setting.enabled ? `Sent weekly, next ${v.setting.nextAt ? when(v.setting.nextAt, v.setting.timezone) : ""}.` : "Off. The project's owner can switch it on."}
        </p>
      )}
      <div>
        <h3 className="mb-1 text-xs font-medium text-muted-foreground">Sent digests</h3>
        {v.digests.length === 0 ? (
          <p className="text-sm text-muted-foreground">None yet.</p>
        ) : (
          <ul aria-label="Sent digests" className="divide-y rounded-md border text-sm">
            {v.digests.map((d) => (
              <li key={d.id} className="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2">
                <span className="tabular-nums">{when(d.createdAt, v.setting.timezone)}</span>
                <span className="text-xs text-muted-foreground">{d.trigger === "manual" ? "sent on request" : "weekly"}</span>
                <span className="min-w-0 flex-1 truncate text-muted-foreground" title={d.summary}>{d.summary}</span>
                <a href={d.url} target="_blank" rel="noreferrer" className="text-primary underline underline-offset-2">Open report</a>
                <a href={`${d.url}?download=1`} aria-label={`Download the report of ${when(d.createdAt, v.setting.timezone)}`} className="text-muted-foreground hover:text-foreground">
                  <Download className="size-4" aria-hidden />
                </a>
              </li>
            ))}
          </ul>
        )}
      </div>
      <AlertDialog open={quiet !== undefined} onOpenChange={(o) => !o && setQuiet(undefined)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>A quiet week</AlertDialogTitle>
            <AlertDialogDescription>{quiet}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={() => send.mutate({ force: true })}>Send anyway</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </section>
  );
}
