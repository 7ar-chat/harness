import { useQuery } from "@tanstack/react-query";
import { Clock, Loader2, ShieldX } from "lucide-react";
import type { ReactNode } from "react";
import { meQuery, useAuthProviders } from "@/api/queries";
import type { AuthProvider, Me } from "@/api/types";
import { Logo } from "@/components/app/Logo";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { UserAvatar } from "@/components/app/common";

/** The page around the sign-in and approval cards. */
function Screen({ children }: { children: ReactNode }) {
  return (
    <main className="flex min-h-svh flex-col items-center justify-center gap-6 bg-background p-4">
      <div className="flex items-center gap-2 text-xl font-semibold tracking-tight">
        <Logo className="size-7" />
        har
      </div>
      {children}
    </main>
  );
}

const GithubMark = () => (
  <svg viewBox="0 0 16 16" className="size-4" fill="currentColor" aria-hidden="true" focusable="false">
    <path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0 0 16 8c0-4.42-3.58-8-8-8z" />
  </svg>
);

/** The sign-in link of a provider: the page the visitor was on comes back after the round trip. */
export function providerHref(p: AuthProvider, next = location.pathname + location.search + location.hash): string {
  return `${p.url}${p.url.includes("?") ? "&" : "?"}next=${encodeURIComponent(next)}`;
}

/** Shown instead of the app when there is no session (first visit or an expired one). */
export function LoginPage({ expired = false }: { expired?: boolean }) {
  const providers = useAuthProviders();
  return (
    <Screen>
      <Card className="w-full max-w-sm">
        <CardHeader className="text-center">
          <CardTitle>Sign in</CardTitle>
          <CardDescription>{expired ? "Your session expired. Sign in again to continue." : "Sign in to continue to your workspace."}</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-2">
          {providers.isPending ? (
            <div className="flex justify-center py-2" role="status" aria-label="Loading sign-in methods">
              <Loader2 className="size-4 animate-spin text-muted-foreground" />
            </div>
          ) : providers.isError ? (
            <div className="flex flex-col items-center gap-2 text-center text-sm text-muted-foreground">
              <p>Could not load the sign-in methods.</p>
              <Button variant="outline" size="sm" onClick={() => void providers.refetch()}>
                Retry
              </Button>
            </div>
          ) : providers.data.length === 0 ? (
            <p className="text-center text-sm text-muted-foreground">Sign-in is not configured on this server.</p>
          ) : (
            providers.data.map((p) => (
              <Button key={p.id} asChild variant={p.id === "github" ? "default" : "outline"} size="lg">
                <a href={providerHref(p)}>
                  {p.id === "github" ? <GithubMark /> : null}
                  {p.id === "github" ? "Continue with GitHub" : p.id === "oidc" ? "Continue with single sign-on" : `Continue with ${p.name}`}
                </a>
              </Button>
            ))
          )}
          <p className="pt-2 text-center text-xs text-muted-foreground">New accounts are reviewed by an administrator before they get access.</p>
        </CardContent>
      </Card>
    </Screen>
  );
}

export async function signOut() {
  try {
    await fetch("/auth/logout", { method: "POST", credentials: "same-origin" }).catch(() => undefined);
  } finally {
    location.assign("/");
  }
}

/** Shown to someone signed in whose account an administrator has not approved yet. It re-checks every 10 s. */
export function PendingPage({ me }: { me: Me }) {
  // Polling /me is what flips the screen into the app once the account is approved.
  useQuery({ ...meQuery, refetchInterval: 10_000, staleTime: 0 });
  return (
    <Screen>
      <Card className="w-full max-w-sm">
        <CardHeader className="items-center text-center">
          <div className="mb-1 flex size-10 items-center justify-center rounded-full bg-warning/20 text-warning-foreground">
            <Clock className="size-5" />
          </div>
          <CardTitle>Waiting for approval</CardTitle>
          <CardDescription>Your account was created. An administrator has been notified and will approve it shortly. This page updates by itself.</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col items-center gap-4">
          <SignedInAs me={me} />
          <Button variant="outline" onClick={() => void signOut()}>
            Sign out
          </Button>
        </CardContent>
      </Card>
    </Screen>
  );
}

/** Shown to someone whose account an administrator rejected. */
export function RejectedPage({ me }: { me: Me }) {
  return (
    <Screen>
      <Card className="w-full max-w-sm">
        <CardHeader className="items-center text-center">
          <div className="mb-1 flex size-10 items-center justify-center rounded-full bg-destructive/15 text-destructive">
            <ShieldX className="size-5" />
          </div>
          <CardTitle>Access denied</CardTitle>
          <CardDescription>An administrator did not approve this account. Contact them if you think this is a mistake.</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col items-center gap-4">
          <SignedInAs me={me} />
          <Button variant="outline" onClick={() => void signOut()}>
            Sign out
          </Button>
        </CardContent>
      </Card>
    </Screen>
  );
}

function SignedInAs({ me }: { me: Me }) {
  const u = me.user;
  return (
    <div className="flex items-center gap-2 text-left">
      <UserAvatar login={u.login} name={u.name} url={u.avatarUrl} className="size-8" />
      <div className="grid leading-tight">
        <span className="text-sm font-medium">{u.name || u.login}</span>
        <span className="text-xs text-muted-foreground">{u.email || u.login}</span>
      </div>
    </div>
  );
}
