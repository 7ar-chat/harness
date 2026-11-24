// Queries of the Insights tab and page: what the agents delivered and what it cost (GET .../insights, GET /me/insights).
import { useQuery } from "@tanstack/react-query";
import { api } from "./client";
import type { InsightsView } from "./types";

export type { InsightsView };

export const INSIGHT_RANGES = [
  { key: "7d", label: "Last 7 days" },
  { key: "30d", label: "Last 30 days" },
  { key: "90d", label: "Last 90 days" },
] as const;

/** The caller's time zone, so a "day" is their day. */
export const localZone = () => {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch {
    return "UTC";
  }
};

const query = (range: string) => `range=${encodeURIComponent(range)}&tz=${encodeURIComponent(localZone())}`;

export const insightsKey = (projectId: string | undefined, range: string) => ["insights", projectId ?? "me", range] as const;

/** A project's insights; without a project, the caller's own across projects. */
export const useInsights = (projectId: string | undefined, range: string) =>
  useQuery({
    queryKey: insightsKey(projectId, range),
    queryFn: () => api.get<InsightsView>(projectId ? `/projects/${projectId}/insights?${query(range)}` : `/me/insights?${query(range)}`),
    retry: false,
    staleTime: 30_000,
  });

/** Where the exportable report of a project is served (the browser opens or saves it: the session cookie authenticates). */
export const reportUrl = (projectId: string, range: string, format: "html" | "csv" | "json", download = false) =>
  `/api/projects/${projectId}/insights/report?${query(range)}&format=${format}${download ? "&download=1" : ""}`;
