// Recipes: a gallery of ready-made schedules, tasks and triggers you start in two clicks, and the ones your team shares.
import { Link } from "@tanstack/react-router";
import { BookOpen, CalendarClock, ChevronDown, ChevronRight, Copy, Download, Pencil, ListChecks, Plus, Search, Trash2, Upload, Webhook } from "lucide-react";
import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { toast } from "sonner";
import { ApiError } from "@/api/client";
import { useMe } from "@/api/queries";
import { useSchedulePreview } from "@/api/schedulesUsage";
import {
  CATEGORIES, CATEGORY_LABEL, SCOPE_LABEL, checkValues, exportRecipe, fillTemplate, useApplyRecipe, useDeleteRecipe, useDuplicateRecipe, useImportRecipe, useRecipes, useShareRecipe, valuesOf,
  type Param, type Recipe, type RecipeApplied, type RecipeKind, type RecipeScope,
} from "@/api/recipes";
import { Empty, ErrorState, ListSkeleton } from "@/components/app/common";
import { RecipeEditor } from "@/pages/RecipeEditor";
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { cn } from "@/lib/utils";

const KIND_LABEL: Record<string, string> = { schedule: "Schedule", task: "Task", trigger: "Trigger" };
const KIND_ICON: Record<string, ReactNode> = {
  schedule: <CalendarClock className="size-4" />,
  task: <ListChecks className="size-4" />,
  trigger: <Webhook className="size-4" />,
};
/** Where a created thing lives, for the "Open" link after Create. */
const KIND_TAB: Record<string, [string, string]> = { schedule: ["schedules", "Schedules"], task: ["tasks", "Tasks"], trigger: ["tasks", "Tasks & triggers"] };

const CRON_CHOICES: { label: string; cron: string }[] = [
  { label: "Every hour", cron: "0 * * * *" },
  { label: "Every day at 09:00", cron: "0 9 * * *" },
  { label: "Weekdays at 09:00", cron: "0 9 * * 1-5" },
  { label: "Every Monday at 09:00", cron: "0 9 * * 1" },
  { label: "Every Friday at 17:00", cron: "0 17 * * 5" },
];

function Chip({ active, onClick, children }: { active: boolean; onClick: () => void; children: ReactNode }) {
  return (
    <button
      type="button"
      aria-pressed={active}
      onClick={onClick}
      className={cn(
        "rounded-full border px-3 py-1 text-xs font-medium whitespace-nowrap transition-colors focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none",
        active ? "border-primary bg-primary text-primary-foreground" : "bg-background text-muted-foreground hover:bg-accent hover:text-foreground",
      )}
    >
      {children}
    </button>
  );
}

/** One recipe as a card: what it is, what it makes, what it may cost, and the way in. */
function RecipeCard({ r, onUse, canUse, onDelete, onEdit, onDuplicate }: { r: Recipe; onUse: (r: Recipe) => void; canUse: boolean; onDelete?: (r: Recipe) => void; onEdit: (r: Recipe) => void; onDuplicate: (r: Recipe) => void }) {
  return (
    <li className="flex min-w-0 flex-col gap-2 rounded-lg border bg-card p-4" aria-label={r.name}>
      <div className="flex items-start gap-2">
        <span className="mt-0.5 text-muted-foreground" title={KIND_LABEL[r.kind]}>
          {KIND_ICON[r.kind]}
        </span>
        <h3 className="min-w-0 flex-1 text-sm leading-snug font-semibold">{r.name}</h3>
        {r.scope !== "builtin" ? <Badge variant="secondary">{r.scope === "project" ? (r.projectName ?? SCOPE_LABEL.project) : SCOPE_LABEL[r.scope]}</Badge> : null}
      </div>
      <p className="text-sm text-muted-foreground">{r.summary}</p>
      <p className="text-xs">
        <span className="font-medium">Makes:</span> <span className="text-muted-foreground">{r.creates}</span>
      </p>
      {r.cost ? <p className="text-xs text-muted-foreground">{r.cost}</p> : null}
      <div className="mt-auto flex items-center gap-1.5 pt-2">
        <Button size="sm" onClick={() => onUse(r)} disabled={!canUse} title={canUse ? undefined : "Members can use recipes"}>
          Use this recipe
        </Button>
        <Button size="icon" variant="ghost" aria-label={`Export ${r.name}`} title="Export as a file" onClick={() => void exportRecipe(r).catch((e: unknown) => toast.error(e instanceof Error ? e.message : "Export failed"))}>
          <Download />
        </Button>
        {r.canEdit ? (
          <Button size="icon" variant="ghost" aria-label={`Edit ${r.name}`} title="Edit" onClick={() => onEdit(r)}>
            <Pencil />
          </Button>
        ) : null}
        <Button size="icon" variant="ghost" aria-label={`Duplicate ${r.name}`} title="Duplicate to my recipes" onClick={() => onDuplicate(r)}>
          <Copy />
        </Button>
        {r.canEdit && onDelete ? (
          <Button size="icon" variant="ghost" aria-label={`Delete ${r.name}`} onClick={() => onDelete(r)}>
            <Trash2 />
          </Button>
        ) : null}
        {r.uses > 0 ? <span className="ml-auto text-xs text-muted-foreground">used {r.uses}×</span> : null}
      </div>
    </li>
  );
}

/** The gallery: search, category chips, and the cards. `kind` fixes what is shown (the Schedules tab only offers schedules). */
export function RecipeGallery({ kind, canUse, onUse, onDelete }: { kind?: RecipeKind; canUse: boolean; onUse: (r: Recipe) => void; onDelete?: (r: Recipe) => void }) {
  const q = useRecipes();
  const [text, setText] = useState("");
  const [cat, setCat] = useState("");
  const [only, setOnly] = useState<"" | RecipeKind>("");
  const [mine, setMine] = useState(false);
  const [editing, setEditing] = useState<Recipe | null>(null);
  const dup = useDuplicateRecipe();
  const shown = useMemo(() => {
    const t = text.trim().toLowerCase();
    return (q.data ?? []).filter(
      (r) =>
        (!kind || r.kind === kind) &&
        (!only || r.kind === only) &&
        (!cat || r.category === cat) &&
        (!mine || r.scope !== "builtin") &&
        (!t || `${r.name} ${r.summary} ${r.category} ${r.kind}`.toLowerCase().includes(t)),
    );
  }, [q.data, text, cat, only, mine, kind]);
  if (q.isLoading) return <ListSkeleton rows={3} />;
  if (q.error) return <ErrorState error={q.error} retry={() => void q.refetch()} />;
  if (!q.data) return <Empty title="No recipes here">This server has no recipe library.</Empty>;
  const fixed = (q.data ?? []).filter((r) => !kind || r.kind === kind);
  return (
    <div className="min-w-0 space-y-4">
      <div className="flex flex-wrap items-center gap-2">
        <div className="relative min-w-48 flex-1 sm:max-w-xs">
          <Search className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input value={text} onChange={(e) => setText(e.target.value)} placeholder="Search recipes" aria-label="Search recipes" className="pl-8" />
        </div>
        {!kind ? (
          <div className="flex flex-wrap gap-1.5" role="group" aria-label="Kind">
            <Chip active={only === ""} onClick={() => setOnly("")}>All kinds</Chip>
            {(["schedule", "task", "trigger"] as const).map((k) => (
              <Chip key={k} active={only === k} onClick={() => setOnly(only === k ? "" : k)}>
                {KIND_LABEL[k]}s
              </Chip>
            ))}
          </div>
        ) : null}
        <Chip active={mine} onClick={() => setMine(!mine)}>
          Shared by people
        </Chip>
      </div>
      <div className="flex flex-wrap gap-1.5" role="group" aria-label="Category">
        <Chip active={cat === ""} onClick={() => setCat("")}>All</Chip>
        {CATEGORIES.filter((c) => fixed.some((r) => r.category === c)).map((c) => (
          <Chip key={c} active={cat === c} onClick={() => setCat(cat === c ? "" : c)}>
            {CATEGORY_LABEL[c]}
          </Chip>
        ))}
      </div>
      {shown.length ? (
        <ul className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-3" aria-label="Recipes">
          {shown.map((r) => (
            <RecipeCard key={r.id} r={r} canUse={canUse} onUse={onUse} onDelete={onDelete} onEdit={setEditing} onDuplicate={(x) => dup.mutate(x)} />
          ))}
        </ul>
      ) : (
        <Empty icon={<BookOpen className="size-6" />} title={text || cat || mine ? "No recipe matches" : "No recipes yet"}>
          {text || cat || mine ? "Try another word or clear the filters." : "Share a schedule or a task as a recipe, or import a recipe file."}
        </Empty>
      )}
      {editing ? <RecipeEditor key={editing.id} recipe={editing} onClose={() => setEditing(null)} /> : null}
    </div>
  );
}

// ---- the wizard ----

function ParamField({ p, value, error, onChange }: { p: Param; value: string; error?: string; onChange: (v: string) => void }) {
  const id = `rp-${p.key}`;
  const effective = value !== "" ? value : (p.default ?? "");
  let input: ReactNode;
  switch (p.type) {
    case "choice":
      input = (
        <Select value={effective} onValueChange={onChange}>
          <SelectTrigger id={id} className="w-full" aria-label={p.label}>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {(p.choices ?? []).map((c) => (
              <SelectItem key={c} value={c}>
                {c}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      );
      break;
    case "boolean":
      input = <Switch id={id} checked={effective === "true"} onCheckedChange={(v) => onChange(v ? "true" : "false")} aria-label={p.label} />;
      break;
    case "number":
      input = <Input id={id} type="number" value={value} placeholder={p.default} onChange={(e) => onChange(e.target.value)} aria-invalid={!!error} />;
      break;
    case "cron": {
      const preset = CRON_CHOICES.find((c) => c.cron === effective)?.cron ?? "custom";
      input = (
        <div className="grid gap-2 sm:grid-cols-2">
          <Select value={preset} onValueChange={(v) => v !== "custom" && onChange(v)}>
            <SelectTrigger className="w-full" aria-label={`${p.label}: common choices`}>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {CRON_CHOICES.map((c) => (
                <SelectItem key={c.cron} value={c.cron}>
                  {c.label}
                </SelectItem>
              ))}
              <SelectItem value="custom">Custom</SelectItem>
            </SelectContent>
          </Select>
          <Input id={id} value={value} placeholder={p.default} onChange={(e) => onChange(e.target.value)} className="font-mono" aria-invalid={!!error} aria-label={p.label} />
        </div>
      );
      break;
    }
    default:
      input =
        (p.default ?? "").length > 90 || value.includes("\n") ? (
          <Textarea id={id} rows={3} value={value} placeholder={p.default} onChange={(e) => onChange(e.target.value)} aria-invalid={!!error} />
        ) : (
          <Input id={id} value={value} placeholder={p.default} onChange={(e) => onChange(e.target.value)} aria-invalid={!!error} />
        );
  }
  return (
    <div className="space-y-1.5">
      <Label htmlFor={id}>
        {p.label}
        {p.required ? <span className="text-destructive"> *</span> : null}
      </Label>
      {input}
      {error ? <p className="text-xs text-destructive">{error}</p> : p.help ? <p className="text-xs text-muted-foreground">{p.help}</p> : null}
    </div>
  );
}

const SCHEDULE_POLICIES: [string, string][] = [
  ["default", "Normal"],
  ["readonly", "Read-only (plan mode)"],
];
const TASK_POLICIES: [string, string][] = [
  ["supervised", "Supervised: every risky call asks"],
  ["sandboxed", "Sandboxed: push and merge ask"],
  ["autonomous", "Autonomous: only protected rules deny"],
];

function NextRuns({ cron, timezone }: { cron: string; timezone: string }) {
  const p = useSchedulePreview(cron, timezone);
  if (p.error) return <p className="text-xs text-destructive">{p.error.message}</p>;
  if (!p.data) return null;
  const fmt = (t: string) => {
    try {
      return new Date(t).toLocaleString(undefined, { weekday: "short", day: "numeric", month: "short", hour: "2-digit", minute: "2-digit", hour12: false, timeZone: timezone });
    } catch {
      return new Date(t).toLocaleString();
    }
  };
  return <p className="text-xs text-muted-foreground">Next runs: {p.data.times.slice(0, 3).map(fmt).join(" · ")}</p>;
}

/** The short wizard: only the parameters, a live preview of what will be created, Advanced folded. */
export function RecipeWizard({ projectId, recipe, canCreate, onClose }: { projectId: string; recipe: Recipe | null; canCreate: boolean; onClose: () => void }) {
  const apply = useApplyRecipe(projectId);
  const [typed, setTyped] = useState<Record<string, string>>({});
  const [adv, setAdv] = useState({ role: "", policy: "", maxUsd: "", timeoutMinutes: "", baseBranch: "", model: "" });
  const [advOpen, setAdvOpen] = useState(false);
  const [done, setDone] = useState<RecipeApplied | null>(null);
  const [problem, setProblem] = useState("");
  useEffect(() => {
    if (!recipe) return;
    setTyped(Object.fromEntries(recipe.params.filter((p) => p.default).map((p) => [p.key, p.default as string])));
    setDone(null);
    setProblem("");
    setAdvOpen(false);
    setAdv({ role: recipe.suggest.role ?? "", policy: recipe.suggest.policy ?? "", maxUsd: recipe.suggest.maxUsd ? String(recipe.suggest.maxUsd) : "", timeoutMinutes: recipe.suggest.timeoutMinutes ? String(recipe.suggest.timeoutMinutes) : "", baseBranch: recipe.suggest.baseBranch ?? "", model: "" });
  }, [recipe]);
  if (!recipe) return null;
  const r = recipe;
  const values = valuesOf(r.params, typed);
  const errors = checkValues(r.params, values);
  const ready = Object.keys(errors).length === 0;
  const fill = (s?: string) => fillTemplate(s ?? "", r.params, values);
  const b = r.body;
  const isSchedule = r.kind === "schedule";
  const cron = fill(b.cron);
  const zone = fill(b.timezone) || "UTC";
  const policies = isSchedule ? SCHEDULE_POLICIES : TASK_POLICIES;
  const submit = () => {
    setProblem("");
    const n = (s: string) => (s.trim() === "" ? undefined : Number(s));
    apply.mutate(
      {
        id: r.id,
        input: {
          params: Object.fromEntries(r.params.map((p) => [p.key, values[p.key] ?? ""]).filter(([, v]) => v !== "")),
          advanced: { role: adv.role.trim() || undefined, policy: adv.policy || undefined, maxUsd: n(adv.maxUsd), timeoutMinutes: n(adv.timeoutMinutes), baseBranch: adv.baseBranch.trim() || undefined, model: adv.model.trim() || undefined },
        },
      },
      { onSuccess: setDone, onError: (e) => setProblem(e instanceof ApiError ? e.message : "Could not create it.") },
    );
  };
  const [tab, tabLabel] = KIND_TAB[r.kind] ?? ["schedules", "Schedules"];
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-xl">
        {done ? (
          <div className="space-y-4">
            <DialogHeader>
              <DialogTitle>Done: {r.name}</DialogTitle>
              <DialogDescription>{done.summary}.</DialogDescription>
            </DialogHeader>
            <DialogFooter>
              <Button variant="outline" onClick={onClose}>
                Close
              </Button>
              <Button asChild>
                <Link to="/p/$projectId" params={{ projectId }} search={{ tab: tab as "schedules" | "tasks" }} onClick={onClose}>
                  Open {tabLabel}
                </Link>
              </Button>
            </DialogFooter>
          </div>
        ) : (
          <form
            className="space-y-4"
            onSubmit={(e) => {
              e.preventDefault();
              if (ready && canCreate) submit();
            }}
          >
            <DialogHeader>
              <DialogTitle>{r.name}</DialogTitle>
              <DialogDescription>{r.summary}</DialogDescription>
            </DialogHeader>
            {r.params.length ? (
              <div className="space-y-3">
                {r.params.map((p) => (
                  <ParamField key={p.key} p={p} value={typed[p.key] ?? ""} error={errors[p.key]} onChange={(v) => setTyped((t) => ({ ...t, [p.key]: v }))} />
                ))}
              </div>
            ) : (
              <p className="text-sm text-muted-foreground">Nothing to fill in: this recipe works as it is.</p>
            )}

            <section aria-label="Preview" className="space-y-2 rounded-md border bg-muted/40 p-3">
              <p className="text-xs font-medium tracking-wide text-muted-foreground uppercase">This will create</p>
              <p className="text-sm font-medium">
                {KIND_LABEL[r.kind]}: {fill(b.name || b.title)}
              </p>
              {isSchedule || b.cron ? (
                <p className="text-xs text-muted-foreground">
                  Runs on <code>{cron}</code> ({zone})
                </p>
              ) : b.event ? (
                <p className="text-xs text-muted-foreground">
                  Starts a task when GitHub sends <code>{fill(b.event)}</code>
                </p>
              ) : (
                <p className="text-xs text-muted-foreground">Starts one agent run now, in this project.</p>
              )}
              {isSchedule && cron ? <NextRuns cron={cron} timezone={zone} /> : null}
              {r.kind === "trigger" && b.title ? <p className="text-xs text-muted-foreground">Each task is titled “{fill(b.title)}”.</p> : null}
              <pre className="max-h-40 overflow-auto rounded border bg-background p-2 text-xs whitespace-pre-wrap" aria-label="What the agent will be told">
                {fill(b.prompt)}
              </pre>
              {r.cost ? <p className="text-xs text-muted-foreground">{r.cost}.</p> : null}
            </section>

            <Collapsible open={advOpen} onOpenChange={setAdvOpen}>
              <CollapsibleTrigger className="flex items-center gap-1 text-sm font-medium text-muted-foreground hover:text-foreground">
                {advOpen ? <ChevronDown className="size-4" /> : <ChevronRight className="size-4" />} Advanced
              </CollapsibleTrigger>
              <CollapsibleContent className="mt-3 space-y-3">
                <div className="grid gap-3 sm:grid-cols-2">
                  {!isSchedule ? (
                    <div className="space-y-1.5">
                      <Label htmlFor="rw-role">Role</Label>
                      <Input id="rw-role" value={adv.role} onChange={(e) => setAdv({ ...adv, role: e.target.value })} placeholder="coder" />
                    </div>
                  ) : null}
                  <div className="space-y-1.5">
                    <Label htmlFor="rw-policy">Permissions</Label>
                    <Select value={adv.policy || (isSchedule ? "default" : "sandboxed")} onValueChange={(v) => setAdv({ ...adv, policy: v })}>
                      <SelectTrigger id="rw-policy" className="w-full">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        {policies.map(([v, l]) => (
                          <SelectItem key={v} value={v}>
                            {l}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </div>
                  {isSchedule ? (
                    <div className="space-y-1.5">
                      <Label htmlFor="rw-timeout">Stop after (minutes)</Label>
                      <Input id="rw-timeout" type="number" min={1} max={240} value={adv.timeoutMinutes} onChange={(e) => setAdv({ ...adv, timeoutMinutes: e.target.value })} />
                    </div>
                  ) : (
                    <>
                      <div className="space-y-1.5">
                        <Label htmlFor="rw-budget">Budget (USD)</Label>
                        <Input id="rw-budget" type="number" min="0" step="0.5" value={adv.maxUsd} onChange={(e) => setAdv({ ...adv, maxUsd: e.target.value })} placeholder="No cap" />
                      </div>
                      <div className="space-y-1.5">
                        <Label htmlFor="rw-branch">Base branch</Label>
                        <Input id="rw-branch" value={adv.baseBranch} onChange={(e) => setAdv({ ...adv, baseBranch: e.target.value })} placeholder="The project's default" />
                      </div>
                    </>
                  )}
                  <div className="space-y-1.5">
                    <Label htmlFor="rw-model">Model</Label>
                    <Input id="rw-model" value={adv.model} onChange={(e) => setAdv({ ...adv, model: e.target.value })} placeholder="Server default" />
                  </div>
                </div>
              </CollapsibleContent>
            </Collapsible>

            {problem ? (
              <p className="text-sm text-destructive" role="alert">
                {problem}
              </p>
            ) : null}
            {!canCreate ? <p className="text-sm text-muted-foreground">Only project members can create from a recipe.</p> : null}
            <DialogFooter>
              <Button type="button" variant="outline" onClick={onClose}>
                Cancel
              </Button>
              <Button type="submit" disabled={!ready || !canCreate || apply.isPending}>
                Create
              </Button>
            </DialogFooter>
          </form>
        )}
      </DialogContent>
    </Dialog>
  );
}

/** The "Add from a recipe" button of the Schedules, Tasks and Triggers tabs: the gallery of that kind, then the wizard. */
export function AddFromRecipe({ projectId, kind, canCreate = true, size = "default", variant = "outline" }: { projectId: string; kind: RecipeKind; canCreate?: boolean; size?: "default" | "sm"; variant?: "outline" | "default" }) {
  const [open, setOpen] = useState(false);
  const [chosen, setChosen] = useState<Recipe | null>(null);
  return (
    <>
      <Button size={size} variant={variant} onClick={() => setOpen(true)}>
        <BookOpen /> Add from a recipe
      </Button>
      <Dialog open={open && !chosen} onOpenChange={setOpen}>
        <DialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-5xl">
          <DialogHeader>
            <DialogTitle>Add a {kind} from a recipe</DialogTitle>
            <DialogDescription>Pick one, fill in a few words, and it is running. You can change everything afterwards.</DialogDescription>
          </DialogHeader>
          <RecipeGallery kind={kind} canUse={canCreate} onUse={setChosen} />
        </DialogContent>
      </Dialog>
      <RecipeWizard
        projectId={projectId}
        recipe={chosen}
        canCreate={canCreate}
        onClose={() => {
          setChosen(null);
          setOpen(false);
        }}
      />
    </>
  );
}

// ---- share as a recipe ----

export interface ShareSource {
  from: "schedule" | "task" | "trigger";
  id: string;
  name: string;
  /** The text a person can select to turn a part of it into a parameter. */
  text: string;
}

interface DraftParam {
  key: string;
  label: string;
  help: string;
  text: string;
}

const keyOf = (s: string) => {
  const k = s.toLowerCase().replace(/[^a-z0-9]+/g, "_").replace(/^_+|_+$/g, "").slice(0, 24);
  return /^[a-z]/.test(k) ? k : `p_${k}`.replace(/_$/, "") || "value";
};

/** "Share as recipe": name it, say what it is, and mark the words that should become parameters by selecting them. */
export function ShareRecipeDialog({ projectId, source, onClose }: { projectId: string; source: ShareSource | null; owner?: boolean; onClose: () => void }) {
  const share = useShareRecipe(projectId);
  const me = useMe().data;
  const admin = !!me?.admin;
  const owner = me?.projects.find((p) => p.id === projectId)?.role === "owner";
  const [name, setName] = useState("");
  const [summary, setSummary] = useState("");
  const [category, setCategory] = useState("maintenance");
  const [scope, setScope] = useState<RecipeScope>("personal");
  const [params, setParams] = useState<DraftParam[]>([]);
  const [problem, setProblem] = useState("");
  const [selected, setSelected] = useState("");
  useEffect(() => {
    if (!source) return;
    setName(source.name);
    setSummary("");
    setCategory("maintenance");
    setScope("personal");
    setParams([]);
    setSelected("");
    setProblem("");
  }, [source]);
  if (!source) return null;
  const s = source;
  const look = (el: HTMLTextAreaElement) => setSelected(el.value.slice(el.selectionStart, el.selectionEnd).trim());
  const mark = () => {
    if (!selected || params.some((p) => p.text === selected) || params.length >= 12) return;
    let key = keyOf(selected);
    while (params.some((p) => p.key === key)) key += "_2";
    setParams([...params, { key, label: selected.length > 40 ? `${selected.slice(0, 37)}...` : selected, help: "", text: selected }]);
    setSelected("");
  };
  const setP = (i: number, patch: Partial<DraftParam>) => setParams((ps) => ps.map((p, j) => (j === i ? { ...p, ...patch } : p)));
  const ok = name.trim() !== "" && params.every((p) => /^[a-z][a-z0-9_]{0,31}$/.test(p.key) && p.label.trim() !== "") && new Set(params.map((p) => p.key)).size === params.length;
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-xl">
        <form
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault();
            if (!ok) return;
            setProblem("");
            share.mutate(
              {
                from: s.from, id: s.id, name: name.trim(), summary: summary.trim(), category, scope,
                params: params.map((p) => ({ key: p.key, label: p.label.trim(), help: p.help.trim(), type: "text", text: p.text })),
              },
              { onSuccess: onClose, onError: (e) => setProblem(e instanceof ApiError ? e.message : "Could not share it.") },
            );
          }}
        >
          <DialogHeader>
            <DialogTitle>Share “{s.name}” as a recipe</DialogTitle>
            <DialogDescription>Others can start their own copy in two clicks. Select words in the text below to let people change them.</DialogDescription>
          </DialogHeader>
          <div className="space-y-1.5">
            <Label htmlFor="sr-name">Recipe name</Label>
            <Input id="sr-name" value={name} onChange={(e) => setName(e.target.value)} required />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="sr-summary">What it does, in one line</Label>
            <Input id="sr-summary" value={summary} onChange={(e) => setSummary(e.target.value)} placeholder="Every Monday, summarise last week's merged pull requests" />
          </div>
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="space-y-1.5">
              <Label htmlFor="sr-cat">Category</Label>
              <Select value={category} onValueChange={setCategory}>
                <SelectTrigger id="sr-cat" className="w-full">
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
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="sr-scope">Who can use it</Label>
              <Select value={scope} onValueChange={(v) => setScope(v as RecipeScope)}>
                <SelectTrigger id="sr-scope" className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="personal">Only me</SelectItem>
                  <SelectItem value="project" disabled={!owner}>
                    This project{owner ? "" : " (owners)"}
                  </SelectItem>
                  {admin ? <SelectItem value="instance">Everyone on this server</SelectItem> : null}
                </SelectContent>
              </Select>
            </div>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="sr-text">Text</Label>
            <Textarea id="sr-text" readOnly rows={5} value={s.text} onSelect={(e) => look(e.currentTarget)} onKeyUp={(e) => look(e.currentTarget)} onMouseUp={(e) => look(e.currentTarget)} className="font-mono text-xs" />
            <div className="flex items-center gap-2">
              <Button type="button" size="sm" variant="outline" disabled={!selected} onClick={mark}>
                <Plus /> Make the selection a parameter
              </Button>
              <span className="truncate text-xs text-muted-foreground">{selected ? `“${selected}”` : "Select some words first"}</span>
            </div>
          </div>
          {params.length ? (
            <ul className="space-y-2" aria-label="Parameters">
              {params.map((p, i) => (
                <li key={p.text} className="grid gap-2 rounded-md border p-2 sm:grid-cols-[1fr_1fr_auto]">
                  <Input value={p.label} onChange={(e) => setP(i, { label: e.target.value })} aria-label={`Label for ${p.text}`} placeholder="Label" />
                  <Input value={p.help} onChange={(e) => setP(i, { help: e.target.value })} aria-label={`Help for ${p.text}`} placeholder="A hint for the person (optional)" />
                  <Button type="button" size="icon" variant="ghost" aria-label={`Remove ${p.label}`} onClick={() => setParams(params.filter((_, j) => j !== i))}>
                    <Trash2 />
                  </Button>
                  <p className="text-xs text-muted-foreground sm:col-span-3">
                    Default: “{p.text}” · written <code>{`{{${p.key}}}`}</code>
                  </p>
                </li>
              ))}
            </ul>
          ) : null}
          {problem ? (
            <p className="text-sm text-destructive" role="alert">
              {problem}
            </p>
          ) : null}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" disabled={!ok || share.isPending}>
              Share
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

// ---- import ----

function ImportDialog({ projectId, owner, open, onClose }: { projectId: string; owner: boolean; open: boolean; onClose: () => void }) {
  const imp = useImportRecipe();
  const admin = !!useMe().data?.admin;
  const [scope, setScope] = useState<RecipeScope>("personal");
  const [problem, setProblem] = useState("");
  const input = useRef<HTMLInputElement>(null);
  const read = async (file: File | undefined) => {
    if (!file) return;
    setProblem("");
    if (file.size > 128 << 10) return setProblem("A recipe file is at most 128 KB.");
    let parsed: unknown;
    try {
      parsed = JSON.parse(await file.text());
    } catch {
      return setProblem("This file is not a recipe: it is not valid JSON.");
    }
    imp.mutate({ file: parsed, scope, projectId }, { onSuccess: onClose, onError: (e) => setProblem(e instanceof ApiError ? e.message : "Could not import it.") });
  };
  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Import a recipe</DialogTitle>
          <DialogDescription>Choose a recipe file someone sent you (a .recipe.json). It is checked before anything is saved.</DialogDescription>
        </DialogHeader>
        <div className="space-y-1.5">
          <Label htmlFor="imp-scope">Keep it</Label>
          <Select value={scope} onValueChange={(v) => setScope(v as RecipeScope)}>
            <SelectTrigger id="imp-scope" className="w-full">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="personal">For me only</SelectItem>
              <SelectItem value="project" disabled={!owner}>
                In this project{owner ? "" : " (owners)"}
              </SelectItem>
              {admin ? <SelectItem value="instance">For everyone on this server</SelectItem> : null}
            </SelectContent>
          </Select>
        </div>
        <input ref={input} type="file" accept=".json,application/json" aria-label="Recipe file" className="text-sm file:mr-3 file:rounded-md file:border file:bg-background file:px-3 file:py-1.5 file:text-sm" onChange={(e) => void read(e.target.files?.[0])} />
        {problem ? (
          <p className="text-sm text-destructive" role="alert">
            {problem}
          </p>
        ) : null}
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            Close
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/** The project's Recipes tab. */
export function RecipesPanel({ projectId, canEdit, owner }: { projectId: string; canEdit: boolean; owner: boolean }) {
  const [chosen, setChosen] = useState<Recipe | null>(null);
  const [importing, setImporting] = useState(false);
  const [doomed, setDoomed] = useState<Recipe | null>(null);
  const del = useDeleteRecipe();
  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="max-w-2xl text-sm text-muted-foreground">
          Ready-made recurring jobs and hand-offs. Pick one, fill in a few words, and it becomes a schedule, a task or a trigger of this project. To share your own, use <em>Share as recipe</em> on a schedule or a task.
        </p>
        <Button variant="outline" onClick={() => setImporting(true)} disabled={!canEdit}>
          <Upload /> Import a file
        </Button>
      </div>
      <RecipeGallery canUse={canEdit} onUse={setChosen} onDelete={setDoomed} />
      <RecipeWizard projectId={projectId} recipe={chosen} canCreate={canEdit} onClose={() => setChosen(null)} />
      <ImportDialog projectId={projectId} owner={owner} open={importing} onClose={() => setImporting(false)} />
      <AlertDialog open={!!doomed} onOpenChange={(o) => !o && setDoomed(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete “{doomed?.name}”?</AlertDialogTitle>
            <AlertDialogDescription>People can no longer start it. Schedules, tasks and triggers made from it stay.</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={() => doomed && del.mutate(doomed.id)}>Delete</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
