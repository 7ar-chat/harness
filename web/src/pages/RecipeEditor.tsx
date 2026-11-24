// The recipe editor: change a recipe you may edit, see what it would make while you type, and go back to an older version.
// The server checks every draft (POST /recipes/check); the editor only shows what it says.
import { ArrowDown, ArrowUp, Copy, History, Plus, Trash2 } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { ApiError, api } from "@/api/client";
import { CATEGORIES, CATEGORY_LABEL, bodyOf, checkRecipe, recipeText, recipesKey, useDeleteRecipe, useDuplicateRecipe, useRecipeVersions, useSaveRecipe, type Recipe } from "@/api/recipes";
import type { Body, Param, RecipeBody, RecipeCheck } from "@/api/types";
import { DiffBlock } from "@/chat/DiffBlock";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { useQueryClient } from "@tanstack/react-query";

const KIND_LABEL: Record<string, string> = { schedule: "Schedule", task: "Task", trigger: "Trigger" };
const TYPE_LABEL: Record<string, string> = { text: "Text", number: "Number", choice: "Choice", boolean: "Yes / no", cron: "Schedule (cron)" };
const POLICIES: Record<string, [string, string][]> = {
  schedule: [["default", "Normal"], ["readonly", "Read-only (plan mode)"]],
  other: [["supervised", "Supervised: every risky call asks"], ["sandboxed", "Sandboxed: push and merge ask"], ["autonomous", "Autonomous: only protected rules deny"]],
};

interface DraftParam {
  key: string;
  label: string;
  help: string;
  type: string;
  choices: string;
  default: string;
  required: boolean;
}
interface Draft {
  name: string;
  summary: string;
  category: string;
  kind: string;
  bodyName: string;
  title: string;
  prompt: string;
  when: "cron" | "event";
  cron: string;
  timezone: string;
  event: string;
  role: string;
  policy: string;
  maxUsd: string;
  params: DraftParam[];
}

const draftOf = (r: Recipe): Draft => ({
  name: r.name, summary: r.summary, category: r.category, kind: r.kind,
  bodyName: r.body.name ?? "", title: r.body.title ?? "", prompt: r.body.prompt ?? "",
  when: r.body.event ? "event" : "cron", cron: r.body.cron ?? "", timezone: r.body.timezone ?? "", event: r.body.event ?? "",
  role: r.suggest.role ?? "", policy: r.suggest.policy ?? "", maxUsd: r.suggest.maxUsd ? String(r.suggest.maxUsd) : "",
  params: r.params.map((p) => ({ key: p.key, label: p.label, help: p.help ?? "", type: p.type, choices: (p.choices ?? []).join(", "), default: p.default ?? "", required: !!p.required })),
});

/** The draft as the routes want it: only the fields that belong to its kind are sent. */
export function bodyFromDraft(d: Draft, base: Recipe): RecipeBody {
  const body: Body = {};
  if (d.kind === "schedule") Object.assign(body, { name: d.bodyName, prompt: d.prompt, cron: d.cron, timezone: d.timezone });
  else if (d.kind === "task") Object.assign(body, { title: d.title, prompt: d.prompt });
  else {
    Object.assign(body, { name: d.bodyName, title: d.title, prompt: d.prompt });
    if (d.when === "event") Object.assign(body, { event: d.event, match: base.body.match });
    else body.cron = d.cron;
  }
  const params: Param[] = d.params.map((p) => ({
    key: p.key, label: p.label, type: p.type, ...(p.help ? { help: p.help } : {}), ...(p.default ? { default: p.default } : {}), ...(p.required ? { required: true } : {}),
    ...(p.type === "choice" ? { choices: p.choices.split(",").map((c) => c.trim()).filter(Boolean) } : {}),
  }));
  const n = Number(d.maxUsd);
  const suggest = { ...(d.role.trim() ? { role: d.role.trim() } : {}), ...(d.policy ? { policy: d.policy } : {}), ...(d.maxUsd.trim() && n > 0 ? { maxUsd: n } : {}), ...(base.suggest.maxTokens ? { maxTokens: base.suggest.maxTokens } : {}), ...(base.suggest.timeoutMinutes ? { timeoutMinutes: base.suggest.timeoutMinutes } : {}), ...(base.suggest.baseBranch ? { baseBranch: base.suggest.baseBranch } : {}) };
  return bodyOf({ name: d.name, summary: d.summary, category: d.category, kind: d.kind, body, params, suggest }, { scope: base.scope, projectId: base.scopeId ?? "", version: base.version });
}

function Field({ id, label, children, hint }: { id: string; label: string; children: React.ReactNode; hint?: string }) {
  return (
    <div className="space-y-1.5">
      <Label htmlFor={id}>{label}</Label>
      {children}
      {hint ? <p className="text-xs text-muted-foreground">{hint}</p> : null}
    </div>
  );
}

const when = (t: string) => {
  try {
    return new Date(t).toLocaleString(undefined, { day: "numeric", month: "short", year: "numeric", hour: "2-digit", minute: "2-digit", hour12: false });
  } catch {
    return t;
  }
};

/** Older versions of a recipe: restore one (it becomes a new version) or compare two. */
function HistoryPanel({ base, busy, onRestore }: { base: Recipe; busy: boolean; onRestore: (v: Recipe) => void }) {
  const q = useRecipeVersions(base.id, true);
  const all = useMemo(() => [{ version: base.version, recipe: base as Recipe, changedBy: undefined as string | undefined, at: base.updatedAt }, ...(q.data?.history ?? []).map((h) => ({ ...h, recipe: h.recipe as Recipe }))], [base, q.data]);
  const [from, setFrom] = useState("");
  const [to, setTo] = useState("");
  const a = all.find((v) => String(v.version) === (from || String(all[1]?.version ?? ""))) ?? all[1];
  const b = all.find((v) => String(v.version) === (to || String(base.version))) ?? all[0];
  return (
    <section aria-label="Version history" className="min-w-0 space-y-3 border-t pt-4 lg:border-t-0 lg:border-l lg:pt-0 lg:pl-4">
      <h3 className="text-sm font-semibold">History</h3>
      {q.isLoading ? <p className="text-sm text-muted-foreground">Loading</p> : null}
      {q.error ? <p className="text-sm text-destructive">Could not load the history.</p> : null}
      {q.data && all.length === 1 ? <p className="text-sm text-muted-foreground">No older versions yet. Each save keeps the one before it.</p> : null}
      <ul className="space-y-1.5" aria-label="Versions">
        {all.map((v, i) => (
          <li key={v.version} className="flex items-center gap-2 rounded-md border px-2 py-1.5 text-sm" aria-label={`Version ${v.version}`}>
            <div className="min-w-0 flex-1">
              <p className="font-medium">
                Version {v.version}
                {i === 0 ? <span className="ml-1.5 text-xs font-normal text-muted-foreground">current</span> : null}
              </p>
              <p className="truncate text-xs text-muted-foreground">
                Saved {when(v.at)}
                {v.changedBy ? ` · replaced by ${v.changedBy}` : ""}
              </p>
            </div>
            {i > 0 ? (
              <Button type="button" size="sm" variant="outline" disabled={busy} aria-label={`Restore version ${v.version}`} onClick={() => onRestore(v.recipe)}>
                Restore this version
              </Button>
            ) : null}
          </li>
        ))}
      </ul>
      {all.length > 1 && a && b ? (
        <div className="space-y-2">
          <div className="grid grid-cols-2 gap-2">
            <Field id="cmp-from" label="Compare">
              <Select value={String(a.version)} onValueChange={setFrom}>
                <SelectTrigger id="cmp-from" className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {all.map((v) => (
                    <SelectItem key={v.version} value={String(v.version)}>
                      Version {v.version}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </Field>
            <Field id="cmp-to" label="with">
              <Select value={String(b.version)} onValueChange={setTo}>
                <SelectTrigger id="cmp-to" className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {all.map((v) => (
                    <SelectItem key={v.version} value={String(v.version)}>
                      Version {v.version}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </Field>
          </div>
          {a.version === b.version ? (
            <p className="text-sm text-muted-foreground">Pick two different versions.</p>
          ) : (
            <div aria-label={`Changes from version ${a.version} to version ${b.version}`}>
              <DiffBlock path="recipe" oldText={recipeText(a.recipe)} newText={recipeText(b.recipe)} />
            </div>
          )}
        </div>
      ) : null}
    </section>
  );
}

/** The editor dialog. `recipe` is one the caller may edit (Recipe.canEdit). */
export function RecipeEditor({ recipe, onClose }: { recipe: Recipe; onClose: () => void }) {
  const qc = useQueryClient();
  const [base, setBase] = useState(recipe);
  const [d, setD] = useState<Draft>(() => draftOf(recipe));
  const [showHistory, setShowHistory] = useState(false);
  const [conflict, setConflict] = useState(false);
  const [problem, setProblem] = useState("");
  const [check, setCheck] = useState<RecipeCheck | null>(null);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const save = useSaveRecipe();
  const del = useDeleteRecipe();
  const dup = useDuplicateRecipe();
  const set = (p: Partial<Draft>) => setD((x) => ({ ...x, ...p }));
  const setP = (i: number, p: Partial<DraftParam>) => setD((x) => ({ ...x, params: x.params.map((q, j) => (j === i ? { ...q, ...p } : q)) }));
  const load = (r: Recipe) => {
    setBase(r);
    setD(draftOf(r));
    setConflict(false);
    setProblem("");
  };
  const body = useMemo(() => bodyFromDraft(d, base), [d, base]);
  const seq = useRef(0);
  useEffect(() => {
    const mine = ++seq.current;
    const t = setTimeout(() => {
      checkRecipe(body, {})
        .then((r) => mine === seq.current && setCheck(r))
        .catch(() => mine === seq.current && setCheck(null));
    }, 300);
    return () => clearTimeout(t);
  }, [body]);
  const saveBody = (b: RecipeBody) => {
    setProblem("");
    setConflict(false);
    save.mutate(
      { id: base.id, body: b },
      {
        onSuccess: (r) => load(r),
        onError: (e) => {
          if (e instanceof ApiError && e.status === 409) setConflict(true);
          else setProblem(e instanceof ApiError ? e.message : "Could not save it.");
        },
      },
    );
  };
  const reload = () =>
    void api.get<Recipe>(`/recipes/${base.id}`).then((r) => {
      load(r);
      void qc.invalidateQueries({ queryKey: recipesKey });
    });
  const restore = (v: Recipe) => saveBody(bodyOf(v, { scope: base.scope, projectId: base.scopeId ?? "", version: base.version }));
  const dirty = JSON.stringify(body) !== JSON.stringify(bodyFromDraft(draftOf(base), base));
  const invalid = check ? (check.ok ? "" : (check.error ?? "")) : "";
  const policies = POLICIES[d.kind === "schedule" ? "schedule" : "other"]!;
  const paramProblem = d.params.some((p) => p.type === "choice" && p.choices.split(",").filter((c) => c.trim()).length < 2) ? "A choice needs at least two options." : "";
  return (
    <>
      <Dialog open onOpenChange={(o) => !o && onClose()}>
        <DialogContent className={`max-h-[90svh] overflow-y-auto ${showHistory ? "sm:max-w-5xl" : "sm:max-w-3xl"}`}>
          <DialogHeader>
            <DialogTitle>Edit “{base.name}”</DialogTitle>
            <DialogDescription>
              Version {base.version}. Saving keeps the previous version in the history. Write <code>{"{{key}}"}</code> in the text where a parameter goes.
            </DialogDescription>
          </DialogHeader>
          <div className={showHistory ? "grid gap-4 lg:grid-cols-[minmax(0,1fr)_20rem]" : ""}>
            <form
              className="min-w-0 space-y-4"
              onSubmit={(e) => {
                e.preventDefault();
                if (!invalid && !paramProblem && dirty) saveBody(body);
              }}
            >
              <div className="grid gap-3 sm:grid-cols-2">
                <Field id="re-name" label="Name">
                  <Input id="re-name" value={d.name} onChange={(e) => set({ name: e.target.value })} maxLength={80} />
                </Field>
                <Field id="re-kind" label="Kind">
                  <Select value={d.kind} onValueChange={(v) => set({ kind: v, policy: "" })}>
                    <SelectTrigger id="re-kind" className="w-full">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {Object.entries(KIND_LABEL).map(([k, l]) => (
                        <SelectItem key={k} value={k}>
                          {l}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </Field>
              </div>
              <Field id="re-summary" label="What it does, in one line">
                <Input id="re-summary" value={d.summary} onChange={(e) => set({ summary: e.target.value })} maxLength={300} />
              </Field>
              <Field id="re-cat" label="Category">
                <Select value={d.category} onValueChange={(v) => set({ category: v })}>
                  <SelectTrigger id="re-cat" className="w-full sm:w-64">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {CATEGORIES.map((c) => (
                      <SelectItem key={c} value={c}>
                        {CATEGORY_LABEL[c]}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </Field>

              <fieldset className="space-y-3 rounded-md border p-3">
                <legend className="px-1 text-sm font-medium">{KIND_LABEL[d.kind]}</legend>
                {d.kind !== "task" ? (
                  <Field id="re-bname" label={d.kind === "schedule" ? "Schedule name" : "Trigger name"}>
                    <Input id="re-bname" value={d.bodyName} onChange={(e) => set({ bodyName: e.target.value })} />
                  </Field>
                ) : null}
                {d.kind !== "schedule" ? (
                  <Field id="re-title" label={d.kind === "task" ? "Task title" : "Title of each task"}>
                    <Input id="re-title" value={d.title} onChange={(e) => set({ title: e.target.value })} />
                  </Field>
                ) : null}
                <Field id="re-prompt" label={d.kind === "schedule" ? "Prompt" : "Spec"} hint={d.kind === "schedule" ? "What the agent is told on every run." : "What the task asks for."}>
                  <Textarea id="re-prompt" rows={6} value={d.prompt} onChange={(e) => set({ prompt: e.target.value })} className="font-mono text-xs" />
                </Field>
                {d.kind === "trigger" ? (
                  <div className="flex gap-2" role="group" aria-label="Starts">
                    <Button type="button" size="sm" variant={d.when === "cron" ? "default" : "outline"} aria-pressed={d.when === "cron"} onClick={() => set({ when: "cron" })}>
                      On a schedule
                    </Button>
                    <Button type="button" size="sm" variant={d.when === "event" ? "default" : "outline"} aria-pressed={d.when === "event"} onClick={() => set({ when: "event" })}>
                      On a GitHub event
                    </Button>
                  </div>
                ) : null}
                {d.kind === "schedule" || (d.kind === "trigger" && d.when === "cron") ? (
                  <div className="grid gap-3 sm:grid-cols-2">
                    <Field id="re-cron" label="Cron" hint="Five fields, for example 0 9 * * 1. A {{key}} works here too.">
                      <Input id="re-cron" value={d.cron} onChange={(e) => set({ cron: e.target.value })} className="font-mono" />
                    </Field>
                    {d.kind === "schedule" ? (
                      <Field id="re-tz" label="Time zone" hint="For example Europe/Paris.">
                        <Input id="re-tz" value={d.timezone} onChange={(e) => set({ timezone: e.target.value })} />
                      </Field>
                    ) : null}
                  </div>
                ) : null}
                {d.kind === "trigger" && d.when === "event" ? (
                  <Field id="re-event" label="GitHub event" hint="For example issues.opened.">
                    <Input id="re-event" value={d.event} onChange={(e) => set({ event: e.target.value })} className="font-mono" />
                  </Field>
                ) : null}
                <div className="grid gap-3 sm:grid-cols-3">
                  <Field id="re-policy" label="Policy">
                    <Select value={d.policy || "none"} onValueChange={(v) => set({ policy: v === "none" ? "" : v })}>
                      <SelectTrigger id="re-policy" className="w-full">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="none">Default</SelectItem>
                        {policies.map(([v, l]) => (
                          <SelectItem key={v} value={v}>
                            {l}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </Field>
                  {d.kind !== "schedule" ? (
                    <Field id="re-role" label="Role">
                      <Input id="re-role" value={d.role} onChange={(e) => set({ role: e.target.value })} placeholder="Project default" />
                    </Field>
                  ) : null}
                  <Field id="re-budget" label={d.kind === "schedule" ? "Budget per run (USD)" : "Budget (USD)"}>
                    <Input id="re-budget" type="number" min="0" step="0.1" value={d.maxUsd} onChange={(e) => set({ maxUsd: e.target.value })} />
                  </Field>
                </div>
              </fieldset>

              <fieldset className="space-y-2">
                <legend className="text-sm font-medium">Parameters</legend>
                <p className="text-xs text-muted-foreground">What the person fills in when starting the recipe. At most 12.</p>
                {d.params.length ? (
                  <ul className="space-y-2" aria-label="Parameters">
                    {d.params.map((p, i) => (
                      <li key={i} className="space-y-2 rounded-md border p-2" aria-label={`Parameter ${p.key || i + 1}`}>
                        <div className="grid gap-2 sm:grid-cols-[8rem_1fr_9rem]">
                          <Input value={p.key} onChange={(e) => setP(i, { key: e.target.value })} aria-label={`Key of parameter ${i + 1}`} placeholder="key" className="font-mono" />
                          <Input value={p.label} onChange={(e) => setP(i, { label: e.target.value })} aria-label={`Label of parameter ${i + 1}`} placeholder="Label" />
                          <Select value={p.type} onValueChange={(v) => setP(i, { type: v })}>
                            <SelectTrigger className="w-full" aria-label={`Type of parameter ${i + 1}`}>
                              <SelectValue />
                            </SelectTrigger>
                            <SelectContent>
                              {Object.entries(TYPE_LABEL).map(([k, l]) => (
                                <SelectItem key={k} value={k}>
                                  {l}
                                </SelectItem>
                              ))}
                            </SelectContent>
                          </Select>
                        </div>
                        <div className="grid gap-2 sm:grid-cols-2">
                          <Input value={p.default} onChange={(e) => setP(i, { default: e.target.value })} aria-label={`Default of parameter ${i + 1}`} placeholder="Default value" />
                          <Input value={p.help} onChange={(e) => setP(i, { help: e.target.value })} aria-label={`Hint of parameter ${i + 1}`} placeholder="A hint (optional)" />
                        </div>
                        {p.type === "choice" ? <Input value={p.choices} onChange={(e) => setP(i, { choices: e.target.value })} aria-label={`Options of parameter ${i + 1}`} placeholder="Options, separated by commas" /> : null}
                        <div className="flex items-center gap-2">
                          <Switch checked={p.required} onCheckedChange={(v) => setP(i, { required: v })} aria-label={`Parameter ${i + 1} is required`} />
                          <span className="text-xs text-muted-foreground">Required</span>
                          <span className="ml-auto flex gap-1">
                            <Button type="button" size="icon" variant="ghost" aria-label={`Move parameter ${i + 1} up`} disabled={i === 0} onClick={() => setD((x) => ({ ...x, params: swap(x.params, i, i - 1) }))}>
                              <ArrowUp />
                            </Button>
                            <Button type="button" size="icon" variant="ghost" aria-label={`Move parameter ${i + 1} down`} disabled={i === d.params.length - 1} onClick={() => setD((x) => ({ ...x, params: swap(x.params, i, i + 1) }))}>
                              <ArrowDown />
                            </Button>
                            <Button type="button" size="icon" variant="ghost" aria-label={`Remove parameter ${i + 1}`} onClick={() => setD((x) => ({ ...x, params: x.params.filter((_, j) => j !== i) }))}>
                              <Trash2 />
                            </Button>
                          </span>
                        </div>
                      </li>
                    ))}
                  </ul>
                ) : null}
                <Button type="button" size="sm" variant="outline" disabled={d.params.length >= 12} onClick={() => setD((x) => ({ ...x, params: [...x.params, { key: "", label: "", help: "", type: "text", choices: "", default: "", required: false }] }))}>
                  <Plus /> Add a parameter
                </Button>
              </fieldset>

              <section aria-label="Check and preview" aria-live="polite" className="space-y-1.5 rounded-md border bg-muted/40 p-3 text-sm">
                {paramProblem ? <p className="text-destructive">{paramProblem}</p> : null}
                {invalid ? (
                  <p className="text-destructive" role="alert">
                    {invalid}
                  </p>
                ) : check?.preview ? (
                  <>
                    <p>
                      <span className="font-medium">Preview with the default values:</span> {check.preview.summary}
                    </p>
                    <pre className="max-h-40 overflow-auto rounded border bg-background p-2 text-xs whitespace-pre-wrap">{check.preview.rendered.body.prompt}</pre>
                  </>
                ) : check?.error ? (
                  <p className="text-muted-foreground">Preview needs a value: {check.error}</p>
                ) : (
                  <p className="text-muted-foreground">Checking</p>
                )}
              </section>
              {conflict ? (
                <div className="flex flex-wrap items-center gap-2 rounded-md border border-destructive/50 p-3 text-sm" role="alert">
                  <span className="min-w-0 flex-1">Someone changed this recipe meanwhile. Reload it and try again.</span>
                  <Button type="button" size="sm" variant="outline" onClick={reload}>
                    Load the latest version
                  </Button>
                </div>
              ) : null}
              {problem ? (
                <p className="text-sm text-destructive" role="alert">
                  {problem}
                </p>
              ) : null}
              <DialogFooter className="gap-2 sm:justify-between">
                <div className="flex flex-wrap gap-2">
                  <Button type="button" variant="outline" aria-pressed={showHistory} onClick={() => setShowHistory(!showHistory)}>
                    <History /> History
                  </Button>
                  <Button type="button" variant="outline" disabled={dup.isPending} onClick={() => dup.mutate(base)}>
                    <Copy /> Duplicate to my recipes
                  </Button>
                  <Button type="button" variant="outline" onClick={() => setConfirmDelete(true)}>
                    <Trash2 /> Delete
                  </Button>
                </div>
                <div className="flex gap-2">
                  <Button type="button" variant="outline" onClick={onClose}>
                    Close
                  </Button>
                  <Button type="submit" disabled={!!invalid || !!paramProblem || !dirty || save.isPending}>
                    Save
                  </Button>
                </div>
              </DialogFooter>
            </form>
            {showHistory ? <HistoryPanel base={base} busy={save.isPending} onRestore={restore} /> : null}
          </div>
        </DialogContent>
      </Dialog>
      <AlertDialog open={confirmDelete} onOpenChange={setConfirmDelete}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete “{base.name}”?</AlertDialogTitle>
            <AlertDialogDescription>People can no longer start it, and its history goes too. Schedules, tasks and triggers made from it stay.</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                setShowHistory(false); // its list would be asked for again as soon as the recipe is gone
                del.mutate(base.id, { onSuccess: onClose });
              }}
            >
              Delete
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}

function swap<T>(a: T[], i: number, j: number): T[] {
  const out = [...a];
  [out[i], out[j]] = [out[j]!, out[i]!];
  return out;
}
