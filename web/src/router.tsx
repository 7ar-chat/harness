import { createHashHistory, createRootRoute, createRoute, createRouter, Link } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { AppShell } from "@/components/app/AppShell";
import { ApiError } from "@/api/client";
import { useMe } from "@/api/queries";
import { Logo } from "@/components/app/Logo";
import { LoginPage, PendingPage, RejectedPage } from "@/components/app/AuthScreens";
import { Empty, ErrorState, Spinner } from "@/components/app/common";
import { Button } from "@/components/ui/button";
import { AdminPage } from "@/pages/Admin";
import { Dashboard } from "@/pages/Dashboard";
import { MemoryPage } from "@/pages/Memory";
import { ProfilePage } from "@/pages/Profile";
import { ProjectPage } from "@/pages/ProjectPage";
import { SessionPage, SupervisorPage } from "@/pages/SessionPage";
import { FeedProvider } from "@/lib/feed";
import { reportError } from "@/lib/telemetry";

/** Full-page loader shown until we know who the visitor is, so a signed-out visit never flashes the interface (or its errors). */
function PageLoader() {
  return (
    <div className="flex h-svh flex-col items-center justify-center gap-3 text-muted-foreground" role="status" aria-busy="true">
      <Logo className="size-8" />
      <Spinner />
    </div>
  );
}

/** Decides what a visitor sees: a loader, the sign-in page, the approval screens or the app. */
function AuthGate({ children }: { children: React.ReactNode }) {
  const me = useMe();
  const [expired, setExpired] = useState(false);
  // A call that answers 401 later on (the session ended): the app gives way to the sign-in page.
  useEffect(() => {
    const h = () => setExpired(true);
    window.addEventListener("harness:unauthorized", h);
    return () => window.removeEventListener("harness:unauthorized", h);
  }, []);
  const unauthenticated = me.error instanceof ApiError && me.error.status === 401;
  if (unauthenticated || expired) return <LoginPage expired={expired && !unauthenticated} />;
  if (me.isPending) return <PageLoader />;
  const status = me.data?.user.status;
  if (me.data && status === "pending") return <PendingPage me={me.data} />;
  if (me.data && status === "rejected") return <RejectedPage me={me.data} />;
  return children;
}

function Root() {
  return (
    <AuthGate>
      <FeedProvider>
        <AppShell />
      </FeedProvider>
    </AuthGate>
  );
}

/** The router's own error screen: a route that throws while loading or rendering ends up here, not in the boundary. */
function RouteError({ error }: { error: unknown }) {
  useEffect(() => reportError(error), [error]);
  return <ErrorState error={error} />;
}

const root = createRootRoute({
  component: Root,
  errorComponent: ({ error }) => <RouteError error={error} />,
  notFoundComponent: () => (
    <Empty title="Page not found">
      <Button asChild variant="outline" className="mt-2">
        <Link to="/">Back to the dashboard</Link>
      </Button>
    </Empty>
  ),
});

const tabs = ["overview", "members", "memory", "capabilities", "audit", "settings"] as const;
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
  createRoute({ getParentRoute: () => root, path: "/admin", component: AdminPage }),
]);

export const router = createRouter({ routeTree, history: createHashHistory(), defaultPreload: "intent" });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
