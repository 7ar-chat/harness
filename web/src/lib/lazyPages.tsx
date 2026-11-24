// Heavy pages load on first use, so the first screen ships a lighter bundle. Each wrapper shows a spinner while its chunk arrives.
import { lazy, Suspense, type ComponentType } from "react";
import { Spinner } from "@/components/app/common";

export const PageFallback = () => (
  <div className="flex h-full min-h-24 items-center justify-center p-6" role="status" aria-label="Loading page">
    <Spinner />
  </div>
);

// eslint-disable-next-line @typescript-eslint/no-explicit-any
export function lazyPage<M, K extends keyof M>(load: () => Promise<M>, name: K): M[K] {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const L = lazy(async () => ({ default: (await load())[name] as unknown as ComponentType<any> }));
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const Wrapped = (props: any) => (
    <Suspense fallback={<PageFallback />}>
      <L {...props} />
    </Suspense>
  );
  return Wrapped as unknown as M[K];
}

export const AdminPage = lazyPage(() => import("@/pages/Admin"), "AdminPage");
export const HelpPage = lazyPage(() => import("@/pages/Help"), "HelpPage");
export const InsightsPage = lazyPage(() => import("@/pages/Insights"), "InsightsPage");
export const JobsPage = lazyPage(() => import("@/pages/Jobs"), "JobsPage");
export { MemoryPage } from "@/pages/Memory"; // also used by the project page and the session page, so it is not a chunk of its own
export const ProfilePage = lazyPage(() => import("@/pages/Profile"), "ProfilePage");
export const ReferencesPage = lazyPage(() => import("@/pages/Profile"), "ReferencesPage");
export const ConnectorsPage = lazyPage(() => import("@/pages/Profile"), "ConnectorsPage");
export const CapabilitiesPage = lazyPage(() => import("@/pages/Profile"), "CapabilitiesPage");
export const RolesPage = lazyPage(() => import("@/pages/Roles"), "RolesPage");
export const SchedulesPage = lazyPage(() => import("@/pages/Schedules"), "SchedulesPage");
export const UsagePage = lazyPage(() => import("@/pages/Usage"), "UsagePage");
export const RecipesPanel = lazyPage(() => import("@/pages/Recipes"), "RecipesPanel");
export const AddFromRecipe = lazyPage(() => import("@/pages/Recipes"), "AddFromRecipe");
export const ShareRecipeDialog = lazyPage(() => import("@/pages/Recipes"), "ShareRecipeDialog");
export const ProjectNotifications = lazyPage(() => import("@/pages/Notifications"), "ProjectNotifications");
export const InsightsPanel = lazyPage(() => import("@/pages/Insights"), "InsightsPanel");
export const WorkflowsPanel = lazyPage(() => import("@/pages/Workflows"), "WorkflowsPanel");
export const Requirements = lazyPage(() => import("@/pages/Requirements"), "Requirements");
export const RolesPanel = lazyPage(() => import("@/pages/Roles"), "RolesPanel");
export const SchedulesPanel = lazyPage(() => import("@/pages/Schedules"), "SchedulesPanel");
export const UsagePanel = lazyPage(() => import("@/pages/Usage"), "UsagePanel");
export const CapabilitiesPanel = lazyPage(() => import("@/components/app/capabilities"), "CapabilitiesPanel");
