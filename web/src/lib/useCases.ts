// Use cases are ready-made setups built on top of the platform, not part of the platform itself. Each one is a row here: the Dashboard
// lists them as cards and the palette offers them. To add one, add a row (and, if the server may lack it, a capability below).
import { useJobsAvailable } from "@/api/queries";

/** What the server can do that a use case needs. */
export type Capability = "jobs";

export interface UseCase {
  id: string;
  title: string;
  /** What it does, in one line. */
  blurb: string;
  /** Where it opens. */
  route: string;
  /** The server capability it needs: without it the use case is not offered. */
  needs: Capability;
  /** The palette's name for it. */
  command: string;
}

export const USE_CASES: UseCase[] = [
  {
    id: "job-search",
    title: "Job search assistant",
    blurb: "Finds and tracks openings from your CV.",
    route: "/jobs",
    needs: "jobs",
    command: "Use case: Job search",
  },
];

/** The use cases this server offers (none while it is still being asked). */
export function useUseCases(): UseCase[] {
  const caps: Record<Capability, boolean> = { jobs: useJobsAvailable() };
  return USE_CASES.filter((u) => caps[u.needs]);
}
