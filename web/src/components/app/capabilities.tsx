import { CheckCircle2, ExternalLink, Plug, Plus, Search, Trash2, X } from "lucide-react";
import { useMemo, useState, type ReactNode } from "react";
import { useAddCapability, useCapabilities, useDeleteCapability, usePutCapability, useTestCapability } from "@/api/queries";
import type { CapabilityPut, CapabilityTestResult, CapabilityTransport, CapabilityView } from "@/api/types";
import { Empty, ErrorState, ListSkeleton, Spinner } from "@/components/app/common";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from "@/components/ui/card";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";

export type CapabilitiesScope = { kind: "project"; projectId: string; canEdit: boolean } | { kind: "user" };

// ---- validation (mirrors the server) ----
const NAME_RE = /^[a-z][a-z0-9_-]{0,31}$/;
const HEADER_RE = /^[!#$%&'*+.^_`|~0-9A-Za-z-]+$/;

export function validateName(name: string): string | undefined {
  if (!NAME_RE.test(name)) return "Use 1 to 32 characters: a lowercase letter first, then lowercase letters, digits, - or _.";
  if (name.includes("__")) return "A name cannot contain two underscores in a row.";
  return undefined;
}

export function validateUrl(raw: string): string | undefined {
  let u: URL;
  try {
    u = new URL(raw.trim());
  } catch {
    return "Enter a full URL, for example https://mcp.example.com/mcp.";
  }
  return u.protocol === "https:" ? undefined : "Only https:// URLs are allowed.";
}

interface HeaderRow {
  id: number;
  name: string;
  value: string;
}
let rowId = 0;

/** Filled rows as a header map, or the first problem found. Blank rows are ignored. */
function headersOf(rows: HeaderRow[], taken: string[] = []): { headers: Record<string, string>; error?: string } {
  const headers: Record<string, string> = {};
  const seen = new Set(taken.map((n) => n.toLowerCase()));
  for (const r of rows) {
    const name = r.name.trim();
    if (!name && !r.value) continue;
    if (!HEADER_RE.test(name)) return { headers, error: `"${name}" is not a valid header name.` };
    if (!r.value) return { headers, error: `Give header ${name} a value.` };
    if (seen.has(name.toLowerCase())) return { headers, error: `Header ${name} appears twice.` };
    seen.add(name.toLowerCase());
    headers[name] = r.value;
  }
  return { headers };
}

// ---- status ----
function statusOf(c: CapabilityView): { label: string; className?: string; variant: "default" | "secondary" | "outline" } {
  if (c.orphan) return { label: "No longer in catalog", variant: "outline" };
  if (c.missing?.length) return { label: "Needs setup", variant: "default", className: "bg-warning text-black" };
  if (c.enabled) return c.defaultEnabled && !c.installed ? { label: "On by default", variant: "secondary" } : { label: "On", variant: "default" };
  return { label: "Off", variant: "outline" };
}

const missingLabels = (c: CapabilityView): string[] => (c.missing ?? []).map((k) => c.fields?.find((f) => f.key === k)?.label ?? k);

// ---- small editors ----
function FieldRow({ id, label, help, required, isSet, value, onValue, cleared, onClear, secret = true, setText = "Set" }: {
  id: string;
  label: string;
  help?: string;
  required?: boolean;
  isSet: boolean;
  value: string;
  onValue: (v: string) => void;
  cleared: boolean;
  onClear: (c: boolean) => void;
  secret?: boolean;
  setText?: string;
}) {
  const helpId = `${id}-help`;
  return (
    <div className="space-y-1.5">
      <div className="flex flex-wrap items-center gap-2">
        <Label htmlFor={id}>{label}</Label>
        {required ? <span className="text-xs text-muted-foreground">required</span> : null}
        {isSet && !cleared ? (
          <Badge variant="secondary" className="gap-1 font-normal">
            <CheckCircle2 /> {setText}
          </Badge>
        ) : null}
        {cleared ? <Badge variant="outline">Will be cleared</Badge> : null}
      </div>
      <div className="flex gap-2">
        <Input
          id={id}
          type={secret ? "password" : "text"}
          autoComplete="off"
          value={value}
          disabled={cleared}
          placeholder={isSet ? "Enter a new value to replace it" : ""}
          aria-describedby={help ? helpId : undefined}
          onChange={(e) => onValue(e.target.value)}
        />
        {isSet ? (
          <Button type="button" variant="outline" aria-label={`${cleared ? "Keep" : "Clear"} ${label}`} onClick={() => { onClear(!cleared); onValue(""); }}>
            {cleared ? "Keep" : "Clear"}
          </Button>
        ) : null}
      </div>
      {help ? (
        <p id={helpId} className="text-xs text-muted-foreground">
          {help}
        </p>
      ) : null}
    </div>
  );
}

function HeaderEditor({ rows, onChange, id }: { rows: HeaderRow[]; onChange: (r: HeaderRow[]) => void; id: string }) {
  const set = (i: number, patch: Partial<HeaderRow>) => onChange(rows.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  return (
    <div className="space-y-2">
      {rows.map((r, i) => (
        <div key={r.id} className="flex items-center gap-2" role="group" aria-label={`Header ${i + 1}`}>
          <Input aria-label={`Header ${i + 1} name`} placeholder="Name, e.g. Authorization" value={r.name} autoComplete="off" onChange={(e) => set(i, { name: e.target.value })} />
          <Input aria-label={`Header ${i + 1} value`} type="password" placeholder="Value" value={r.value} autoComplete="off" onChange={(e) => set(i, { value: e.target.value })} />
          <Button type="button" variant="ghost" size="icon" aria-label={`Remove header ${i + 1}`} onClick={() => onChange(rows.filter((_, j) => j !== i))}>
            <X />
          </Button>
        </div>
      ))}
      <Button id={id} type="button" variant="outline" size="sm" onClick={() => onChange([...rows, { id: ++rowId, name: "", value: "" }])}>
        <Plus /> Add header
      </Button>
    </div>
  );
}

function ToolListEditor({ id, tools, onChange }: { id: string; tools: string[]; onChange: (t: string[]) => void }) {
  const [draft, setDraft] = useState("");
  const [err, setErr] = useState("");
  const add = () => {
    const t = draft.trim();
    if (!t) return;
    if (tools.includes(t)) return setErr(`${t} is already in the list.`);
    onChange([...tools, t]);
    setDraft("");
    setErr("");
  };
  return (
    <div className="space-y-2">
      <Label htmlFor={id}>Allowed tools</Label>
      <div className="flex gap-2">
        <Input
          id={id}
          value={draft}
          placeholder="Tool name"
          autoComplete="off"
          aria-invalid={!!err}
          aria-describedby={`${id}-help`}
          onChange={(e) => { setDraft(e.target.value); setErr(""); }}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              add();
            }
          }}
        />
        <Button type="button" variant="outline" onClick={add} disabled={!draft.trim()}>
          Add tool
        </Button>
      </div>
      {err ? <p role="alert" className="text-xs text-destructive">{err}</p> : null}
      <p id={`${id}-help`} className="text-xs text-muted-foreground">
        {tools.length ? "Only these tools are given to the agent." : "Empty: only the tools the server marks read-only are given to the agent."}
      </p>
      {tools.length ? (
        <ul className="flex flex-wrap gap-1.5" aria-label="Allowed tools">
          {tools.map((t) => (
            <li key={t}>
              <Badge variant="secondary" className="gap-1 font-mono font-normal">
                {t}
                <button type="button" aria-label={`Remove tool ${t}`} className="rounded-full hover:text-destructive" onClick={() => onChange(tools.filter((x) => x !== t))}>
                  <X />
                </button>
              </Badge>
            </li>
          ))}
        </ul>
      ) : (
        <Badge variant="outline" className="font-normal">
          read-only tools
        </Badge>
      )}
    </div>
  );
}

function TestResult({ r }: { r: CapabilityTestResult }) {
  if (!r.ok)
    return (
      <p role="alert" className="rounded-md border border-destructive/50 bg-destructive/10 p-2 text-sm">
        Connection failed{r.error ? `: ${r.error}` : "."}
      </p>
    );
  return (
    <div role="status" className="space-y-2 rounded-md border p-2 text-sm">
      <p className="flex items-center gap-1.5 font-medium">
        <CheckCircle2 className="size-4 text-success" /> Connected. {r.tools?.length ? `${r.tools.length} tool${r.tools.length === 1 ? "" : "s"}:` : "The server offers no tools."}
      </p>
      {r.tools?.length ? (
        <ul className="space-y-1">
          {r.tools.map((t) => (
            <li key={t.name} className="flex flex-wrap items-baseline gap-2">
              <span className="font-mono text-xs">{t.name}</span>
              {t.readOnly ? (
                <Badge variant="secondary" className="font-normal">
                  read-only
                </Badge>
              ) : null}
              {t.description ? <span className="text-xs text-muted-foreground">{t.description}</span> : null}
            </li>
          ))}
        </ul>
      ) : null}
    </div>
  );
}

// ---- configure dialog ----
function ConfigureDialog({ cap, pid, onClose }: { cap: CapabilityView; pid?: string; onClose: () => void }) {
  const put = usePutCapability(pid);
  const del = useDeleteCapability(pid);
  const test = useTestCapability(pid);
  const custom = cap.kind === "custom";
  const fields = cap.fields ?? [];
  const initialTools = cap.tools ?? [];
  const [enabled, setEnabled] = useState(cap.enabled);
  const [blocked, setBlocked] = useState(false);
  const [values, setValues] = useState<Record<string, string>>({});
  const [cleared, setCleared] = useState<Record<string, boolean>>({});
  const [tools, setTools] = useState(initialTools);
  const [url, setUrl] = useState(cap.url);
  const [transport, setTransport] = useState<CapabilityTransport>(cap.transport === "sse" ? "sse" : "http");
  const [hValues, setHValues] = useState<Record<string, string>>({});
  const [hCleared, setHCleared] = useState<Record<string, boolean>>({});
  const [newHeaders, setNewHeaders] = useState<HeaderRow[]>([]);
  const [error, setError] = useState("");
  const [result, setResult] = useState<CapabilityTestResult>();
  const [confirm, setConfirm] = useState(false);
  const busy = put.isPending || del.isPending;
  const headerNames = cap.headerNames ?? [];

  // Required fields that would still be empty after saving what is typed here.
  const missingNow = fields.length
    ? fields.filter((f) => f.required && !(values[f.key]?.trim() || (f.set && !cleared[f.key]))).map((f) => f.label)
    : missingLabels(cap);

  const toggle = (on: boolean) => {
    if (on && missingNow.length) {
      setBlocked(true);
      return;
    }
    setBlocked(false);
    setEnabled(on);
  };

  const save = () => {
    const body: CapabilityPut = { enabled };
    const f: Record<string, string> = {};
    for (const fd of fields) {
      const v = values[fd.key]?.trim();
      if (cleared[fd.key]) f[fd.key] = "";
      else if (v) f[fd.key] = v;
    }
    if (Object.keys(f).length) body.fields = f;
    if (tools.length !== initialTools.length || tools.some((t, i) => t !== initialTools[i])) body.tools = tools;
    if (custom) {
      const u = url.trim();
      if (u !== cap.url) {
        const e = validateUrl(u);
        if (e) return setError(e);
        body.url = u;
      }
      if (transport !== cap.transport) body.transport = transport;
      const added = headersOf(newHeaders, headerNames);
      if (added.error) return setError(added.error);
      const h: Record<string, string> = { ...added.headers };
      for (const n of headerNames) {
        if (hCleared[n]) h[n] = "";
        else if (hValues[n]) h[n] = hValues[n]!;
      }
      if (Object.keys(h).length) body.headers = h;
    }
    setError("");
    put.mutate({ name: cap.name, body }, { onSuccess: onClose });
  };

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[90vh] overflow-y-auto">
        <form
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault();
            if (!cap.orphan) save();
          }}
        >
          <DialogHeader>
            <DialogTitle>{cap.title}</DialogTitle>
            <DialogDescription>{cap.description}</DialogDescription>
          </DialogHeader>
          {cap.orphan ? (
            <p className="rounded-md border border-warning/50 bg-warning/10 p-2 text-sm">This server is no longer in the catalog, so it cannot be used. You can remove it.</p>
          ) : (
            <>
              <div className="space-y-1.5">
                <div className="flex items-center gap-3">
                  <Switch id="cap-enabled" checked={enabled} onCheckedChange={toggle} aria-describedby="cap-enabled-help" />
                  <Label htmlFor="cap-enabled" className="font-normal">
                    Enabled
                  </Label>
                </div>
                <p id="cap-enabled-help" role={blocked ? "alert" : undefined} className={blocked ? "text-sm text-destructive" : "text-xs text-muted-foreground"}>
                  {missingNow.length
                    ? `Set ${missingNow.join(", ")} before enabling.`
                    : cap.defaultEnabled && !cap.installed
                      ? "On by default. Switch it off to opt out."
                      : "Agents can use this server's tools when it is enabled."}
                </p>
              </div>
              {custom ? (
                <>
                  <div className="space-y-1.5">
                    <Label htmlFor="cap-url">URL</Label>
                    <Input id="cap-url" value={url} onChange={(e) => setUrl(e.target.value)} placeholder="https://mcp.example.com/mcp" autoComplete="off" />
                  </div>
                  <div className="space-y-1.5">
                    <Label htmlFor="cap-transport">Transport</Label>
                    <TransportSelect id="cap-transport" value={transport} onChange={setTransport} />
                  </div>
                  {headerNames.map((n) => (
                    <FieldRow
                      key={n}
                      id={`cap-h-${n}`}
                      label={`Header ${n}`}
                      isSet
                      setText="Set"
                      value={hValues[n] ?? ""}
                      onValue={(v) => setHValues({ ...hValues, [n]: v })}
                      cleared={!!hCleared[n]}
                      onClear={(c) => setHCleared({ ...hCleared, [n]: c })}
                    />
                  ))}
                  <div className="space-y-1.5">
                    <Label>{headerNames.length ? "New headers" : "Headers"}</Label>
                    <HeaderEditor id="cap-add-header" rows={newHeaders} onChange={setNewHeaders} />
                  </div>
                </>
              ) : (
                <p className="break-all text-xs text-muted-foreground">
                  Connects to <span className="font-mono">{cap.url}</span>
                </p>
              )}
              {fields.map((f) => (
                <FieldRow
                  key={f.key}
                  id={`cap-f-${f.key}`}
                  label={f.label}
                  help={f.help}
                  required={f.required}
                  secret={f.secret}
                  isSet={f.set}
                  value={values[f.key] ?? ""}
                  onValue={(v) => setValues({ ...values, [f.key]: v })}
                  cleared={!!cleared[f.key]}
                  onClear={(c) => setCleared({ ...cleared, [f.key]: c })}
                />
              ))}
              <ToolListEditor id="cap-tools" tools={tools} onChange={setTools} />
              {error ? (
                <p role="alert" className="text-sm text-destructive">
                  {error}
                </p>
              ) : null}
              <div className="space-y-2 border-t pt-4">
                <div className="flex flex-wrap items-center gap-2">
                  <Button type="button" variant="outline" disabled={test.isPending || !(cap.installed || custom || !cap.missing?.length)} onClick={() => test.mutate(cap.name, { onSuccess: setResult })}>
                    {test.isPending ? <Spinner /> : <Plug />} Test connection
                  </Button>
                  <span className="text-xs text-muted-foreground">Tests the saved settings.</span>
                </div>
                {result ? <TestResult r={result} /> : null}
              </div>
            </>
          )}
          <DialogFooter className="sm:justify-between">
            {custom || cap.orphan || cap.installed ? (
              <Button type="button" variant="ghost" className="text-destructive" disabled={busy} onClick={() => (custom || cap.orphan ? setConfirm(true) : del.mutate(cap.name, { onSuccess: onClose }))}>
                <Trash2 /> {custom || cap.orphan ? "Remove" : "Reset to default"}
              </Button>
            ) : (
              <span />
            )}
            <div className="flex gap-2">
              <Button type="button" variant="ghost" onClick={onClose}>
                {cap.orphan ? "Close" : "Cancel"}
              </Button>
              {cap.orphan ? null : (
                <Button type="submit" disabled={busy || (enabled && missingNow.length > 0)}>
                  {put.isPending ? <Spinner /> : null} Save
                </Button>
              )}
            </div>
          </DialogFooter>
        </form>
        <AlertDialog open={confirm} onOpenChange={setConfirm}>
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>Remove {cap.title}?</AlertDialogTitle>
              <AlertDialogDescription>Agents will no longer be able to use it{custom ? ", and its saved headers are deleted" : ""}.</AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel>Keep it</AlertDialogCancel>
              <AlertDialogAction onClick={() => del.mutate(cap.name, { onSuccess: onClose })}>Remove</AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      </DialogContent>
    </Dialog>
  );
}

function TransportSelect({ id, value, onChange }: { id: string; value: CapabilityTransport; onChange: (t: CapabilityTransport) => void }) {
  return (
    <Select value={value} onValueChange={(v) => onChange(v as CapabilityTransport)}>
      <SelectTrigger id={id} className="w-full">
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        <SelectItem value="http">Streamable HTTP</SelectItem>
        <SelectItem value="sse">SSE</SelectItem>
      </SelectContent>
    </Select>
  );
}

// ---- add dialog ----
function AddDialog({ pid, onClose }: { pid?: string; onClose: () => void }) {
  const add = useAddCapability(pid);
  const [name, setName] = useState("");
  const [url, setUrl] = useState("");
  const [transport, setTransport] = useState<CapabilityTransport>("http");
  const [rows, setRows] = useState<HeaderRow[]>([]);
  const [tools, setTools] = useState<string[]>([]);
  const [enabled, setEnabled] = useState(true);
  const [errors, setErrors] = useState<{ name?: string; url?: string; headers?: string }>({});

  const submit = () => {
    const hs = headersOf(rows);
    const e = { name: validateName(name.trim()), url: validateUrl(url), headers: hs.error };
    setErrors(e);
    if (e.name || e.url || e.headers) return;
    add.mutate(
      { name: name.trim(), url: url.trim(), transport, enabled, ...(Object.keys(hs.headers).length ? { headers: hs.headers } : {}), ...(tools.length ? { tools } : {}) },
      { onSuccess: onClose },
    );
  };
  const err = (id: string, m?: string) =>
    m ? (
      <p id={id} role="alert" className="text-xs text-destructive">
        {m}
      </p>
    ) : null;

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[90vh] overflow-y-auto">
        <form
          noValidate
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault();
            submit();
          }}
        >
          <DialogHeader>
            <DialogTitle>Add server</DialogTitle>
            <DialogDescription>Connect your own MCP server. It must be reachable over https.</DialogDescription>
          </DialogHeader>
          <div className="space-y-1.5">
            <Label htmlFor="cap-new-name">Name</Label>
            <Input id="cap-new-name" value={name} autoComplete="off" autoFocus placeholder="my-server" aria-invalid={!!errors.name} aria-describedby={errors.name ? "cap-new-name-err" : undefined} onChange={(e) => setName(e.target.value)} />
            {err("cap-new-name-err", errors.name)}
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="cap-new-url">URL</Label>
            <Input id="cap-new-url" value={url} autoComplete="off" placeholder="https://mcp.example.com/mcp" aria-invalid={!!errors.url} aria-describedby={errors.url ? "cap-new-url-err" : undefined} onChange={(e) => setUrl(e.target.value)} />
            {err("cap-new-url-err", errors.url)}
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="cap-new-transport">Transport</Label>
            <TransportSelect id="cap-new-transport" value={transport} onChange={setTransport} />
          </div>
          <div className="space-y-1.5">
            <Label>Headers</Label>
            <HeaderEditor id="cap-new-add-header" rows={rows} onChange={setRows} />
            {err("cap-new-headers-err", errors.headers)}
          </div>
          <ToolListEditor id="cap-new-tools" tools={tools} onChange={setTools} />
          <div className="flex items-center gap-3">
            <Switch id="cap-new-enabled" checked={enabled} onCheckedChange={setEnabled} />
            <Label htmlFor="cap-new-enabled" className="font-normal">
              Enable it now
            </Label>
          </div>
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" disabled={add.isPending}>
              {add.isPending ? <Spinner /> : null} Add server
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

// ---- cards and the panel ----
function CapCard({ cap, canEdit, onConfigure }: { cap: CapabilityView; canEdit: boolean; onConfigure: () => void }) {
  const st = statusOf(cap);
  const need = missingLabels(cap);
  return (
    <Card className="h-full">
      <CardHeader>
        <CardTitle className="flex flex-wrap items-center gap-2 text-base">
          <h3>{cap.title}</h3>
          <Badge variant={st.variant} className={st.className}>
            {st.label}
          </Badge>
          {cap.category ? (
            <Badge variant="outline" className="font-normal">
              {cap.category}
            </Badge>
          ) : null}
        </CardTitle>
        <CardDescription>{cap.description}</CardDescription>
      </CardHeader>
      <CardContent className="space-y-1.5 text-xs text-muted-foreground">
        {need.length && !cap.orphan ? <p>Needs: {need.join(", ")}</p> : null}
        {cap.kind === "custom" ? <p className="break-all font-mono">{cap.url}</p> : null}
        <p>{cap.tools?.length ? `${cap.tools.length} allowed tool${cap.tools.length === 1 ? "" : "s"}` : "Read-only tools"}</p>
        {cap.homepage ? (
          <a href={cap.homepage} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 hover:underline" aria-label={`${cap.title} homepage`}>
            <ExternalLink className="size-3" /> Homepage
          </a>
        ) : null}
      </CardContent>
      {canEdit ? (
        <CardFooter>
          <Button variant="outline" size="sm" aria-label={`${cap.orphan ? "Manage" : "Configure"} ${cap.title}`} onClick={onConfigure}>
            {cap.orphan ? "Manage" : "Configure"}
          </Button>
        </CardFooter>
      ) : null}
    </Card>
  );
}

function Section({ id, title, action, children }: { id: string; title: string; action?: ReactNode; children: ReactNode }) {
  return (
    <section className="space-y-3" aria-labelledby={id}>
      <div className="flex flex-wrap items-center gap-2">
        <h2 id={id} className="flex-1 text-sm font-medium text-muted-foreground">
          {title}
        </h2>
        {action}
      </div>
      {children}
    </section>
  );
}

export function CapabilitiesPanel({ scope }: { scope: CapabilitiesScope }) {
  const pid = scope.kind === "project" ? scope.projectId : undefined;
  const canEdit = scope.kind === "user" || scope.canEdit;
  const q = useCapabilities(pid);
  const [search, setSearch] = useState("");
  const [category, setCategory] = useState("all");
  const [selected, setSelected] = useState<string>();
  const [adding, setAdding] = useState(false);
  const list = q.data ?? [];
  const catalog = useMemo(() => list.filter((c) => c.kind === "catalog"), [list]);
  const custom = useMemo(() => list.filter((c) => c.kind === "custom"), [list]);
  const categories = useMemo(() => [...new Set(catalog.map((c) => c.category).filter((c): c is string => !!c))].sort(), [catalog]);
  const shown = useMemo(() => {
    const s = search.trim().toLowerCase();
    return catalog.filter((c) => (category === "all" || c.category === category) && (!s || [c.title, c.name, c.description, c.category ?? ""].some((t) => t.toLowerCase().includes(s))));
  }, [catalog, search, category]);

  if (q.isLoading) return <ListSkeleton />;
  if (q.error) return <ErrorState error={q.error} retry={() => void q.refetch()} />;
  if (q.data === null)
    return (
      <Empty icon={<Plug className="size-8" />} title="Capabilities are not available">
        This server does not offer the MCP marketplace.
      </Empty>
    );
  const sel = list.find((c) => c.name === selected);
  const who = pid ? "this project" : "your personal sessions";

  return (
    <div className="space-y-8">
      <Section id="cap-market-h" title="Marketplace">
        <p className="text-sm text-muted-foreground">
          MCP servers that give agents more tools in {who}.{canEdit ? "" : " Only a project owner can change them."}
        </p>
        <div className="flex flex-wrap gap-2">
          <div className="relative min-w-48 flex-1">
            <Search className="pointer-events-none absolute left-2.5 top-2.5 size-4 text-muted-foreground" />
            <Input type="search" aria-label="Search the marketplace" placeholder="Search servers" className="pl-8" value={search} onChange={(e) => setSearch(e.target.value)} />
          </div>
          {categories.length ? (
            <Select value={category} onValueChange={setCategory}>
              <SelectTrigger aria-label="Filter by category" className="w-44">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all">All categories</SelectItem>
                {categories.map((c) => (
                  <SelectItem key={c} value={c}>
                    {c}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          ) : null}
        </div>
        {shown.length ? (
          <ul className="grid gap-3 sm:grid-cols-2" aria-label="Marketplace servers">
            {shown.map((c) => (
              <li key={c.name}>
                <CapCard cap={c} canEdit={canEdit} onConfigure={() => setSelected(c.name)} />
              </li>
            ))}
          </ul>
        ) : (
          <p className="text-sm text-muted-foreground">{catalog.length ? "No server matches your search." : "The marketplace is empty."}</p>
        )}
      </Section>
      <Section
        id="cap-custom-h"
        title="Added by you"
        action={
          canEdit ? (
            <Button size="sm" onClick={() => setAdding(true)}>
              <Plus /> Add server
            </Button>
          ) : undefined
        }
      >
        {custom.length ? (
          <ul className="grid gap-3 sm:grid-cols-2" aria-label="Custom servers">
            {custom.map((c) => (
              <li key={c.name}>
                <CapCard cap={c} canEdit={canEdit} onConfigure={() => setSelected(c.name)} />
              </li>
            ))}
          </ul>
        ) : (
          <p className="text-sm text-muted-foreground">No custom servers yet.</p>
        )}
      </Section>
      {canEdit && sel ? <ConfigureDialog key={sel.name} cap={sel} pid={pid} onClose={() => setSelected(undefined)} /> : null}
      {canEdit && adding ? <AddDialog pid={pid} onClose={() => setAdding(false)} /> : null}
    </div>
  );
}
