// What the signed-in person has done, counted by the server from the audit log (GET /api/me/activity-summary).
import { useQuery } from "@tanstack/react-query";
import { api, isUnsupported } from "./client";
import type { ActivitySummary } from "./types";

export type { ActivitySummary };

export const activityKey = ["me-activity"] as const;

/** The caller's own counts, or null on a server that keeps none. */
export const useActivitySummary = () =>
  useQuery({
    queryKey: activityKey,
    queryFn: async () => {
      try {
        return await api.get<ActivitySummary>("/me/activity-summary");
      } catch (e) {
        if (isUnsupported(e)) return null;
        throw e;
      }
    },
    staleTime: 60_000,
    retry: false,
  });
