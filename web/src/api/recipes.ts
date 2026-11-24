// Recipes: ready-made schedules, tasks and triggers (the gallery), what people share with their team, and applying one.
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api, downloadText, isUnsupported } from "./client";
import { toastError } from "./queries";
import type { Param, Recipe, RecipeApplied, RecipeBody, RecipeDryRun, RecipeShareBody, RecipeVersions } from "./types";

export type { Param, Recipe, RecipeApplied, RecipeDryRun };
export type RecipeKind = "schedule" | "task" | "trigger";
export type RecipeScope = "personal" | "project" | "instance";

export const CATEGORIES = ["maintenance", "quality", "docs", "reporting", "security", "onboarding"] as const;
export const CATEGORY_LABEL: Record<string, string> = {
  maintenance: "Maintenance",
  quality: "Quality",
  docs: "Docs",
  reporting: "Reporting",
  security: "Security",
  onboarding: "Onboarding",
};
export const SCOPE_LABEL: Record<string, string> = { builtin: "Ready-made", instance: "Whole team", project: "This project", personal: "Mine" };

export const recipesKey = ["recipes"] as const;

/** The recipes the caller may see, or null when this server has none (404/501). */
export const useRecipes = (enabled = true) =>
  useQuery({
    queryKey: recipesKey,
    queryFn: async () => {
      try {
        return await api.get<Recipe[]>("/recipes");
      } catch (e) {
        if (isUnsupported(e)) return null;
        throw e;
      }
    },
    enabled,
    retry: false,
    staleTime: 30_000,
  });

/** The values of the wizard: parameters by key, plus the settings under "Advanced" (empty = the recipe's own). */
export interface ApplyInput {
  params: Record<string, string>;
  dryRun?: boolean;
  advanced?: { role?: string; policy?: string; maxUsd?: number; maxTokens?: number; timeoutMinutes?: number; baseBranch?: string; model?: string };
}

/** What each kind of apply refreshes: the tab the new thing shows up in. */
const KEYS: Record<string, readonly unknown[]> = {
  schedule: ["schedules"],
  task: ["factory"],
  trigger: ["factory"],
};

export const useApplyRecipe = (projectId: string) => {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, input }: { id: string; input: ApplyInput }) => api.post<RecipeApplied>(`/projects/${projectId}/recipes/${id}/apply`, input),
    onSuccess: (r) => {
      void qc.invalidateQueries({ queryKey: KEYS[r.kind] ?? ["schedules"] });
      void qc.invalidateQueries({ queryKey: recipesKey });
    },
    onError: toastError,
  });
};

export const useShareRecipe = (projectId: string) => {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (b: RecipeShareBody) => api.post<Recipe>(`/projects/${projectId}/recipes/from`, b),
    onSuccess: (r) => {
      void qc.invalidateQueries({ queryKey: recipesKey });
      toast.success(`Shared “${r.name}” as a recipe`);
    },
    onError: toastError,
  });
};

export const useDeleteRecipe = () => {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.del(`/recipes/${id}`),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: recipesKey });
      toast.success("Recipe deleted");
    },
    onError: toastError,
  });
};

/** Creates a recipe from an imported file: `file` is the parsed JSON, sent as it is so the server can refuse what it does not know. */
export const useImportRecipe = () => {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ file, scope, projectId }: { file: unknown; scope: RecipeScope; projectId?: string }) =>
      api.post<Recipe>(`/recipes/import?scope=${scope}${scope === "project" && projectId ? `&project=${encodeURIComponent(projectId)}` : ""}`, file),
    onSuccess: (r) => {
      void qc.invalidateQueries({ queryKey: recipesKey });
      toast.success(`Imported “${r.name}”`);
    },
    onError: toastError,
  });
};

export const useSaveRecipe = () => {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, body }: { id: string; body: Partial<RecipeBody> }) => api.put<Recipe>(`/recipes/${id}`, body),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: recipesKey });
      toast.success("Saved");
    },
    onError: toastError,
  });
};

export const useRecipeVersions = (id: string, enabled: boolean) =>
  useQuery({ queryKey: [...recipesKey, id, "versions"] as const, queryFn: () => api.get<RecipeVersions>(`/recipes/${id}/versions`), enabled, retry: false });

export const exportRecipe = (r: Pick<Recipe, "id" | "name">) =>
  downloadText(`/recipes/${r.id}/export`, `${r.name.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "") || "recipe"}.recipe.json`);

// ---- the same one-pass templating as the server, for the live preview ----

const TOKEN = /\{\{\s*([a-z][a-z0-9_]{0,31})\s*\}\}/g;

/** The value each parameter has: what the person typed, else its default. */
export const valuesOf = (params: Param[], typed: Record<string, string>): Record<string, string> => {
  const out: Record<string, string> = {};
  for (const p of params) out[p.key] = (typed[p.key] ?? "").trim() !== "" ? typed[p.key]!.trim() : (p.default ?? "");
  return out;
};

/** Fills {{key}} from values in one pass (a value is never searched again). A key without a value stays visible as «label». */
export const fillTemplate = (text: string, params: Param[], values: Record<string, string>): string =>
  text.replace(TOKEN, (_, k: string) => {
    const v = values[k];
    return v ? v : `«${params.find((p) => p.key === k)?.label ?? k}»`;
  });

/** Why the values cannot be used, per parameter key (the server checks again). */
export function checkValues(params: Param[], values: Record<string, string>): Record<string, string> {
  const bad: Record<string, string> = {};
  for (const p of params) {
    const v = values[p.key] ?? "";
    if (v === "") {
      if (p.required) bad[p.key] = "Required";
      continue;
    }
    if (v.length > 2000) bad[p.key] = "At most 2000 characters";
    else if (p.type === "number" && !/^-?\d{1,12}(\.\d{1,6})?$/.test(v)) bad[p.key] = "Must be a number";
    else if (p.type === "cron" && !v.startsWith("@") && v.split(/\s+/).length !== 5) bad[p.key] = "Five fields, for example 0 2 * * *";
  }
  return bad;
}
