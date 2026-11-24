// Notifications: what is told to whom, where. The project tab (owner: channels, rules, delivery log) and the personal
// card in Settings (each person chooses where their own items reach them) share this file.
import { formatDistanceToNowStrict } from "date-fns";
import { Bell, Plus, Send, Trash2 } from "lucide-react";
import { useEffect, useState } from "react";
import { toast } from "sonner";
import {
  useAddChannel, useDeleteChannel, useMyNotifications, useNotificationLog, useNotifications, useSaveMyNotifications, useSaveRules, useTestChannel, useTestMine, useUpdateChannel,
  type MyInput,
} from "@/api/notifications";
import { errMessage } from "@/api/queries";
import type { NotifyChannel, NotifyEvent, NotifyLogEntry, NotifyRule } from "@/api/types";
import { Empty, ErrorState, ListSkeleton, Spinner } from "@/components/app/common";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";

const ago = (iso: string) => `${formatDistanceToNowStrict(new Date(iso))} ago`;

/** What each kind of channel is for and how to get its address, in plain words. */
export const CHANNEL_KINDS: { kind: string; label: string; purpose: string; steps: string; urlLabel: string; placeholder: string; secret: boolean }[] = [
  {
    kind: "webhook", label: "Webhook", urlLabel: "Webhook address", placeholder: "https://example.com/hooks/7ar", secret: true,
    purpose: "Send a JSON message to any web address, to feed your own tools, a chat bridge or an automation.",
    steps: "Give us the address that should receive a POST. If you set a secret, every message is signed: the X-7ar-Signature header holds sha256= and the HMAC-SHA256 of the body. The body is {event, project, title, url, at, data}.",
  },
  {
    kind: "wecom", label: "WeCom group bot", urlLabel: "Bot webhook address", placeholder: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=...", secret: false,
    purpose: "Post into a WeCom (WeChat Work) group chat.",
    steps: "In the WeCom group, open the group settings, add a group bot, and copy its webhook address. The key in it is kept encrypted and never shown again.",
  },
  {
    kind: "discord", label: "Discord", urlLabel: "Webhook address", placeholder: "https://discord.com/api/webhooks/...", secret: false,
    purpose: "Post into a Discord channel.",
    steps: "In Discord open the channel settings, then Integrations, then Webhooks, create one and copy its address. It is kept encrypted and never shown again.",
  },
];

const VIA_LABEL: Record<string, string> = { inapp: "In the app", telegram: "Telegram", slack: "Slack", email: "Email", webhook: "My webhook" };

function statusBadge(s: string) {
  const tone = s === "failed" ? "bg-destructive/10 text-destructive" : s === "delivered" ? "bg-success/15 text-success" : "bg-muted text-muted-foreground";
  return <span className={`inline-block rounded px-1.5 py-0.5 text-xs font-medium ${tone}`}>{s}</span>;
}

/** Why a message may not have arrived: the reasons the log can show, in plain words. */
export function WhyNot({ log }: { log: NotifyLogEntry[] }) {
  const failed = log.filter((e) => e.status === "failed").length;
  const held = log.filter((e) => e.status === "held").length;
  const collapsed = log.filter((e) => e.status === "collapsed").length;
  return (
    <details className="rounded-md border bg-muted/30 p-3 text-sm">
      <summary className="cursor-pointer font-medium">Why did I not get a message?</summary>
      <ul className="mt-2 list-disc space-y-1 pl-5 text-muted-foreground">
        <li>The event may be off for that channel: check the table above. Quiet events (done, finished, new member) stay in the app unless you turn them on.</li>
        <li>Quiet hours or the hourly digest hold messages for later{held ? ` (${held} held in the list below)` : ""}.</li>
        <li>The same event within the repeat window is sent once{collapsed ? ` (${collapsed} collapsed below)` : ""}.</li>
        <li>The channel may have failed{failed ? ` (${failed} failed below, with the reason)` : ""}: press Test on it.</li>
        <li>Telegram, Slack and email only work once you linked them in My settings and Connectors.</li>
      </ul>
    </details>
  );
}

function Log({ entries, showWho }: { entries: NotifyLogEntry[]; showWho?: boolean }) {
  if (!entries.length) return <Empty title="Nothing sent yet">Every message, held message and failure shows up here.</Empty>;
  return (
    <div className="overflow-x-auto rounded-lg border" tabIndex={0} role="region" aria-label="Delivery log, scrolls sideways">
      <table className="w-full text-sm" aria-label="Delivery log">
        <thead className="bg-muted/40 text-left text-xs text-muted-foreground">
          <tr>
            <th className="p-2">When</th>
            <th className="p-2">Event</th>
            {showWho ? <th className="p-2">To</th> : null}
            <th className="p-2">Channel</th>
            <th className="p-2">Status</th>
          </tr>
        </thead>
        <tbody>
          {entries.map((e) => (
            <tr key={e.id} className={`border-t ${e.status === "failed" ? "bg-destructive/5" : ""}`}>
              <td className="whitespace-nowrap p-2 text-xs text-muted-foreground">{ago(e.at)}</td>
              <td className="p-2">
                <div className="truncate">{e.title}</div>
                <div className="text-xs text-muted-foreground">{e.event}</div>
              </td>
              {showWho ? <td className="p-2 text-xs">{e.login || "the project"}</td> : null}
              <td className="p-2 text-xs">{e.channel}</td>
              <td className="p-2">
                {statusBadge(e.status)}
                {e.error ? <div className="mt-1 text-xs text-destructive">{e.error}</div> : null}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// ---- the project ----

function AddChannel({ pid, onClose }: { pid: string; onClose: () => void }) {
  const add = useAddChannel(pid);
  const test = useTestChannel(pid);
  const [kind, setKind] = useState<string>("");
  const [name, setName] = useState("");
  const [url, setUrl] = useState("");
  const [secret, setSecret] = useState("");
  const spec = CHANNEL_KINDS.find((k) => k.kind === kind);
  const save = () =>
    add.mutate({ kind, name: name.trim(), url: url.trim(), secret: secret || undefined }, {
      onSuccess: (c) => {
        toast.success("Channel added. Sending a test message...");
        test.mutate(c.id, {
          onSuccess: (r) => (r.status === "delivered" ? toast.success("The test message arrived") : toast.error(`The test message failed: ${r.error ?? "unknown error"}`)),
          onError: (e) => toast.error(errMessage(e)),
        });
        onClose();
      },
    });
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Add a channel</DialogTitle>
          <DialogDescription>{spec ? spec.purpose : "Where should this project's messages go?"}</DialogDescription>
        </DialogHeader>
        {!spec ? (
          <ul className="space-y-2">
            {CHANNEL_KINDS.map((k) => (
              <li key={k.kind}>
                <button type="button" onClick={() => setKind(k.kind)} className="w-full rounded-lg border p-3 text-left hover:bg-accent">
                  <div className="text-sm font-medium">{k.label}</div>
                  <div className="text-xs text-muted-foreground">{k.purpose}</div>
                </button>
              </li>
            ))}
          </ul>
        ) : (
          <div className="space-y-3">
            <p className="rounded-md bg-muted/40 p-3 text-xs text-muted-foreground">{spec.steps}</p>
            <div className="space-y-1.5">
              <Label htmlFor="nc-name">Name</Label>
              <Input id="nc-name" value={name} maxLength={60} onChange={(e) => setName(e.target.value)} placeholder={`${spec.label} for the team`} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="nc-url">{spec.urlLabel}</Label>
              <Input id="nc-url" value={url} onChange={(e) => setUrl(e.target.value)} placeholder={spec.placeholder} autoComplete="off" />
            </div>
            {spec.secret ? (
              <div className="space-y-1.5">
                <Label htmlFor="nc-secret">Signing secret (optional)</Label>
                <Input id="nc-secret" type="password" value={secret} onChange={(e) => setSecret(e.target.value)} autoComplete="new-password" />
              </div>
            ) : null}
          </div>
        )}
        <DialogFooter>
          {spec ? (
            <Button variant="ghost" onClick={() => setKind("")}>
              Back
            </Button>
          ) : null}
          <Button variant="outline" onClick={onClose}>
            Cancel
          </Button>
          {spec ? (
            <Button disabled={!name.trim() || !url.trim() || add.isPending} onClick={save}>
              Add and send a test
            </Button>
          ) : null}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function ChannelRow({ pid, c, canEdit }: { pid: string; c: NotifyChannel; canEdit: boolean }) {
  const update = useUpdateChannel(pid);
  const del = useDeleteChannel(pid);
  const test = useTestChannel(pid);
  const [result, setResult] = useState<{ ok: boolean; text: string } | null>(null);
  const kind = CHANNEL_KINDS.find((k) => k.kind === c.kind);
  return (
    <li className="flex flex-wrap items-center gap-3 p-3">
      <div className="min-w-0 flex-1">
        <div className="truncate text-sm font-medium">
          {c.name} <Badge variant="secondary">{kind?.label ?? c.kind}</Badge>
          {c.hasSecret ? <Badge variant="outline" className="ml-1">signed</Badge> : null}
        </div>
        <div className="truncate text-xs text-muted-foreground">{c.hint ? `Sends to ${c.hint}` : "Address hidden"}</div>
        {result ? (
          <div role="status" className={`mt-1 text-xs ${result.ok ? "text-success" : "text-destructive"}`}>
            {result.text}
          </div>
        ) : null}
      </div>
      {canEdit ? (
        <>
          <Switch checked={c.enabled} onCheckedChange={(enabled) => update.mutate({ id: c.id, enabled })} aria-label={`${c.name} enabled`} />
          <Button
            size="sm"
            variant="outline"
            disabled={test.isPending}
            onClick={() => {
              setResult(null);
              test.mutate(c.id, {
                onSuccess: (r) => setResult({ ok: r.status === "delivered", text: r.status === "delivered" ? "Delivered: the test message arrived." : `Failed: ${r.error ?? "unknown error"}` }),
                onError: (e) => setResult({ ok: false, text: errMessage(e) }),
              });
            }}
          >
            {test.isPending ? <Spinner /> : <Send className="size-3.5" />} Test
          </Button>
          <Button size="icon" variant="ghost" aria-label={`Remove ${c.name}`} onClick={() => confirm(`Remove ${c.name}?`) && del.mutate(c.id)}>
            <Trash2 className="size-4" />
          </Button>
        </>
      ) : null}
    </li>
  );
}

const cell = "p-2 text-center";

export function ProjectNotifications({ projectId, canEdit }: { projectId: string; canEdit: boolean }) {
  const q = useNotifications(projectId);
  const log = useNotificationLog(projectId, canEdit);
  const save = useSaveRules(projectId);
  const [adding, setAdding] = useState(false);
  if (q.isLoading) return <ListSkeleton />;
  if (q.error || !q.data) return <ErrorState error={q.error} retry={() => void q.refetch()} />;
  const v = q.data;
  const rules = v.rules;
  const rule = (e: NotifyEvent): NotifyRule => rules.events[e.kind] ?? { channels: [], person: true };
  const put = (events: Record<string, NotifyRule>, extra: Partial<{ dedupeMinutes: number; digest: boolean }> = {}) =>
    save.mutate({ events, dedupeMinutes: extra.dedupeMinutes ?? rules.dedupeMinutes, digest: extra.digest ?? rules.digest });
  const setRule = (e: NotifyEvent, next: NotifyRule) => put({ ...rules.events, [e.kind]: next });
  return (
    <div className="space-y-6">
      <section className="space-y-2">
        <div className="flex items-center justify-between">
          <h2 className="text-sm font-semibold">Channels</h2>
          {canEdit ? (
            <Button size="sm" onClick={() => setAdding(true)} disabled={!v.canSecure} title={v.canSecure ? undefined : "This server has no secret key"}>
              <Plus className="size-4" /> Add channel
            </Button>
          ) : null}
        </div>
        {!v.canSecure && canEdit ? <p className="text-xs text-warning">Channels need HARNESS_SECRET_KEY on the server, so their addresses can be stored encrypted.</p> : null}
        {v.channels.length ? (
          <ul className="divide-y rounded-lg border">
            {v.channels.map((c) => (
              <ChannelRow key={c.id} pid={projectId} c={c} canEdit={canEdit} />
            ))}
          </ul>
        ) : (
          <Empty title="No channels yet">
            A channel is a place the whole team hears about this project: a webhook for your own tools, a WeCom group or a Discord channel. Without one, each person is told in the app and where they linked Telegram, Slack or email.
          </Empty>
        )}
      </section>

      <section className="space-y-2">
        <h2 className="text-sm font-semibold">Rules</h2>
        <div className="overflow-x-auto rounded-lg border" tabIndex={0} role="region" aria-label="Notification rules, scrolls sideways">
          <table className="w-full text-sm" aria-label="Notification rules">
            <thead className="bg-muted/40 text-xs text-muted-foreground">
              <tr>
                <th className="p-2 text-left">When this happens</th>
                <th className={cell}>The person it concerns (as they chose)</th>
                {v.channels.map((c) => (
                  <th key={c.id} className={cell}>
                    {c.name}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {v.events.map((e) => {
                const r = rule(e);
                return (
                  <tr key={e.kind} className="border-t">
                    <td className="p-2">
                      <div>{e.label}</div>
                      <div className="text-xs text-muted-foreground">{e.help}</div>
                    </td>
                    <td className={cell}>
                      <input type="checkbox" aria-label={`${e.label}: the person it concerns`} checked={r.person} disabled={!canEdit} onChange={(ev) => setRule(e, { ...r, person: ev.target.checked })} />
                    </td>
                    {v.channels.map((c) => (
                      <td key={c.id} className={cell}>
                        <input
                          type="checkbox"
                          aria-label={`${e.label}: ${c.name}`}
                          checked={r.channels.includes(c.id)}
                          disabled={!canEdit}
                          onChange={(ev) => setRule(e, { ...r, channels: ev.target.checked ? [...r.channels, c.id] : r.channels.filter((x) => x !== c.id) })}
                        />
                      </td>
                    ))}
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
        <div className="flex flex-wrap items-center gap-6 text-sm">
          <label className="flex items-center gap-2">
            Send the same event once per
            <Input
              type="number" min={0} max={1440} className="w-20" disabled={!canEdit} aria-label="Repeat window in minutes" defaultValue={rules.dedupeMinutes}
              onBlur={(e) => {
                const n = Math.max(0, Math.min(1440, Number(e.target.value) || 0));
                if (n !== rules.dedupeMinutes) put(rules.events, { dedupeMinutes: n });
              }}
            />
            minutes (0 sends every one)
          </label>
          <label className="flex items-center gap-2">
            <Switch checked={rules.digest} disabled={!canEdit} onCheckedChange={(digest) => put(rules.events, { digest })} aria-label="Batch quiet events into an hourly digest" />
            Batch the quiet events in the channels into one message an hour
          </label>
        </div>
        {!canEdit ? <p className="text-xs text-muted-foreground">Only a project owner changes these.</p> : null}
      </section>

      {canEdit ? (
        <section className="space-y-2">
          <h2 className="text-sm font-semibold">Delivery log</h2>
          {log.isLoading ? <ListSkeleton /> : <Log entries={log.data ?? []} showWho />}
          <WhyNot log={log.data ?? []} />
        </section>
      ) : null}
      {adding ? <AddChannel pid={projectId} onClose={() => setAdding(false)} /> : null}
    </div>
  );
}

// ---- the person ----

export function MyNotificationsCard() {
  const q = useMyNotifications();
  const save = useSaveMyNotifications();
  const test = useTestMine();
  const [events, setEvents] = useState<Record<string, string[]>>({});
  const [quiet, setQuiet] = useState({ on: false, start: "22:00", end: "07:00", tz: "" });
  const [digest, setDigest] = useState(false);
  const [hook, setHook] = useState({ url: "", secret: "" });
  const [results, setResults] = useState<{ channel: string; status: string; error?: string }[] | null>(null);
  useEffect(() => {
    const d = q.data;
    if (!d) return;
    setEvents(d.effective);
    setQuiet({ on: d.quietStart !== "", start: d.quietStart || "22:00", end: d.quietEnd || "07:00", tz: d.timezone || Intl.DateTimeFormat().resolvedOptions().timeZone || "" });
    setDigest(d.digest);
  }, [q.data]);
  if (q.isLoading) return <ListSkeleton />;
  if (q.error || !q.data) return <ErrorState error={q.error} retry={() => void q.refetch()} />;
  const d = q.data;
  const cols = d.available;
  const toggle = (kind: string, via: string, on: boolean) =>
    setEvents((cur) => ({ ...cur, [kind]: on ? [...new Set([...(cur[kind] ?? []), via])] : (cur[kind] ?? []).filter((x) => x !== via) }));
  const body = (): MyInput => ({
    events, quietStart: quiet.on ? quiet.start : "", quietEnd: quiet.on ? quiet.end : "", timezone: quiet.on ? quiet.tz : "", digest,
    webhook: hook.url ? { url: hook.url, secret: hook.secret } : undefined,
  });
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <Bell className="size-4" /> Notifications
        </CardTitle>
        <CardDescription>Where the things that concern you reach you. The app always has them; add Telegram or Slack in Connectors, email comes from your profile.</CardDescription>
      </CardHeader>
      <CardContent className="space-y-5">
        <div className="overflow-x-auto rounded-lg border" tabIndex={0} role="region" aria-label="My notifications, scrolls sideways">
          <table className="w-full text-sm" aria-label="My notifications">
            <thead className="bg-muted/40 text-xs text-muted-foreground">
              <tr>
                <th className="p-2 text-left">When this happens</th>
                {cols.map((c) => (
                  <th key={c} className={cell}>
                    {VIA_LABEL[c] ?? c}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {d.events.map((e) => (
                <tr key={e.kind} className="border-t">
                  <td className="p-2">
                    <div>{e.label}</div>
                    <div className="text-xs text-muted-foreground">{e.help}</div>
                  </td>
                  {cols.map((c) => (
                    <td key={c} className={cell}>
                      <input
                        type="checkbox" aria-label={`${e.label}: ${VIA_LABEL[c] ?? c}`} checked={c === "inapp" || (events[e.kind] ?? []).includes(c)} disabled={c === "inapp"}
                        onChange={(ev) => toggle(e.kind, c, ev.target.checked)}
                      />
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        {cols.length === 1 ? <p className="text-xs text-muted-foreground">Only the app is set up for you. Link Telegram or Slack under Connectors{d.email ? "" : ", and add an email address to your profile for mail"}.</p> : null}

        <div className="space-y-3 border-t pt-4 text-sm">
          <label className="flex items-center gap-3">
            <Switch checked={quiet.on} onCheckedChange={(on) => setQuiet({ ...quiet, on })} aria-label="Quiet hours" /> Quiet hours: hold messages outside the app until they end
          </label>
          {quiet.on ? (
            <div className="flex flex-wrap items-center gap-2 pl-12">
              <Input type="time" className="w-28" value={quiet.start} onChange={(e) => setQuiet({ ...quiet, start: e.target.value })} aria-label="Quiet hours start" />
              to
              <Input type="time" className="w-28" value={quiet.end} onChange={(e) => setQuiet({ ...quiet, end: e.target.value })} aria-label="Quiet hours end" />
              <Input className="w-48" value={quiet.tz} onChange={(e) => setQuiet({ ...quiet, tz: e.target.value })} aria-label="Time zone" placeholder="Europe/Paris" />
            </div>
          ) : null}
          <label className="flex items-center gap-3">
            <Switch checked={digest} onCheckedChange={setDigest} aria-label="Hourly digest" /> Batch the quiet events outside the app into one message an hour (urgent ones still arrive at once)
          </label>
        </div>

        <div className="space-y-2 border-t pt-4">
          <div className="text-sm font-medium">My webhook {d.webhook.set ? <Badge variant="secondary">sends to {d.webhook.hint}</Badge> : null}</div>
          <p className="text-xs text-muted-foreground">Optional: a web address that gets your messages as signed JSON. The address is stored encrypted and not shown again.</p>
          <div className="flex flex-wrap gap-2">
            <Input className="min-w-60 flex-1" value={hook.url} onChange={(e) => setHook({ ...hook, url: e.target.value })} placeholder={d.webhook.set ? "Enter a new address to replace it" : "https://example.com/hooks/me"} aria-label="My webhook address" autoComplete="off" />
            <Input className="w-48" type="password" value={hook.secret} onChange={(e) => setHook({ ...hook, secret: e.target.value })} placeholder="Secret (optional)" aria-label="My webhook secret" autoComplete="new-password" />
            {d.webhook.set ? (
              <Button variant="outline" onClick={() => save.mutate({ ...body(), webhook: { clear: true } })}>
                Remove
              </Button>
            ) : null}
          </div>
        </div>

        <div className="flex flex-wrap items-center gap-2 border-t pt-4">
          <Button aria-label="Save notification settings" disabled={save.isPending} onClick={() => save.mutate(body(), { onSuccess: () => setHook({ url: "", secret: "" }) })}>
            Save
          </Button>
          <Button
            variant="outline"
            disabled={test.isPending}
            onClick={() => {
              setResults(null);
              test.mutate(undefined, { onSuccess: (r) => setResults(r.results), onError: (e) => toast.error(errMessage(e)) });
            }}
          >
            {test.isPending ? <Spinner /> : <Send className="size-3.5" />} Send me a test
          </Button>
        </div>
        {results ? (
          results.length ? (
            <ul role="status" className="space-y-1 text-sm">
              {results.map((r) => (
                <li key={r.channel} className={r.status === "delivered" ? "text-success" : "text-destructive"}>
                  {r.channel}: {r.status === "delivered" ? "delivered" : `failed (${r.error ?? "unknown error"})`}
                </li>
              ))}
            </ul>
          ) : (
            <p role="status" className="text-sm text-muted-foreground">Nothing outside the app is set up yet, so there is nothing to test.</p>
          )
        ) : null}

        <div className="space-y-2 border-t pt-4">
          <div className="text-sm font-medium">Recent messages to you</div>
          <Log entries={d.recent} />
          <WhyNot log={d.recent} />
        </div>
      </CardContent>
    </Card>
  );
}
