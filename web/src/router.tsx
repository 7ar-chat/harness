import { createHashHistory, createRootRoute, createRoute, createRouter, Link } from "@tanstack/react-router";
import { AppShell } from "@/components/app/AppShell";
import { Empty, ErrorState } from "@/components/app/common";
import { Button } from "@/components/ui/button";
import { Dashboard } from "@/pages/Dashboard";
import { MemoryPage } from "@/pages/Memory";
import { ProfilePage } from "@/pages/Profile";
import { ProjectPage } from "@/pages/ProjectPage";
import { SessionPage, SupervisorPage } from "@/pages/SessionPage";
import { FeedProvider } from "@/lib/feed";

function Root() {
  return (
    <FeedProvider>
      <AppShell />
    </FeedProvider>
  );
}

const root = createRootRoute({
  component: Root,
  errorComponent: ({ error }) => <ErrorState error={error} />,
  notFoundComponent: () => (
    <Empty title="Page not found">
      <Button asChild variant="outline" className="mt-2">
        <Link to="/">Back to the dashboard</Link>
      </Button>
    </Empty>
  ),
});

const tabs = ["overview", "members", "memory", "audit", "settings"] as const;
type Tab = (typeof tabs)[number];

const routeTree = root.addChildren([
  createRoute({ getParentRoute: () => root, path: "/", component: Dashboard }),
  createRoute({ getParentRoute: () => root, path: "/supervisor", component: SupervisorPage }),
  createRoute({ getParentRoute: () => root, path: "/t/$threadId", component: SessionPage }),
  createRoute({
    getParentRoute: () => root,
    path: "/p/$projectId",
    component: ProjectPage,
    validateSearch: (s: Record<string, unknown>): { tab?: Tab } => ({ tab: tabs.find((t) => t === s.tab) }),
  }),
  createRoute({ getParentRoute: () => root, path: "/memory", component: MemoryPage }),
  createRoute({ getParentRoute: () => root, path: "/profile", component: ProfilePage }),
]);

export const router = createRouter({ routeTree, history: createHashHistory(), defaultPreload: "intent" });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
