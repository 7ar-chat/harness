import { formatDistanceToNowStrict } from "date-fns";
import { Check, ShieldAlert, X } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";
import { useAdminUsers, useDecideUser, useMe } from "@/api/queries";
import type { AdminUser, UserStatus } from "@/api/types";
import { Empty, ErrorState, ListSkeleton, UserAvatar } from "@/components/app/common";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";

const TABS: { value: UserStatus; label: string; empty: string }[] = [
  { value: "pending", label: "Pending", empty: "Nobody is waiting for approval." },
  { value: "approved", label: "Approved", empty: "No approved users." },
  { value: "rejected", label: "Rejected", empty: "No rejected users." },
];

export const providerOf = (login: string) => (login.startsWith("github_") ? "GitHub" : "SSO");

function UserRow({ u }: { u: AdminUser }) {
  const decide = useDecideUser();
  const act = (decision: "approve" | "reject") => decide.mutate({ login: u.login, decision }, { onSuccess: () => toast.success(`${u.name || u.login} ${decision === "approve" ? "approved" : "rejected"}`) });
  return (
    <li className="flex flex-wrap items-center gap-3 rounded-lg border bg-card p-3">
      <UserAvatar login={u.login} name={u.name} url={u.avatarUrl} className="size-9" />
      <div className="grid min-w-0 flex-1 leading-tight">
        <span className="truncate text-sm font-medium">{u.name || u.login}</span>
        <span className="truncate text-xs text-muted-foreground">
          {u.login}
          {u.email ? ` · ${u.email}` : ""}
        </span>
      </div>
      <Badge variant="outline">{providerOf(u.login)}</Badge>
      <span className="text-xs text-muted-foreground" title={u.createdAt}>
        Signed up {formatDistanceToNowStrict(new Date(u.createdAt))} ago
      </span>
      <div className="flex gap-2">
        {u.status !== "approved" ? (
          <Button size="sm" disabled={decide.isPending} onClick={() => act("approve")}>
            <Check /> Approve
          </Button>
        ) : null}
        {u.status !== "rejected" ? (
          <Button size="sm" variant="outline" disabled={decide.isPending} onClick={() => act("reject")}>
            <X /> Reject
          </Button>
        ) : null}
      </div>
    </li>
  );
}

export function AdminPage() {
  const me = useMe().data;
  const [status, setStatus] = useState<UserStatus>("pending");
  const isAdmin = !!me?.admin;
  const users = useAdminUsers(status, { enabled: isAdmin });
  if (!isAdmin) {
    return (
      <Empty icon={<ShieldAlert className="size-8" />} title="Forbidden">
        Only administrators can manage users.
      </Empty>
    );
  }
  const tab = TABS.find((t) => t.value === status)!;
  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto flex max-w-3xl flex-col gap-4 p-4 md:p-6">
        <div>
          <h1 className="text-lg font-semibold">Users</h1>
          <p className="text-sm text-muted-foreground">Approve or reject the people who signed up.</p>
        </div>
        <Tabs value={status} onValueChange={(v) => setStatus(v as UserStatus)}>
          <TabsList>
            {TABS.map((t) => (
              <TabsTrigger key={t.value} value={t.value}>
                {t.label}
              </TabsTrigger>
            ))}
          </TabsList>
        </Tabs>
        {users.isPending ? (
          <ListSkeleton />
        ) : users.isError ? (
          <ErrorState error={users.error} retry={() => void users.refetch()} />
        ) : users.data.length === 0 ? (
          <Empty title={tab.empty} />
        ) : (
          <ul className="flex flex-col gap-2">
            {users.data.map((u) => (
              <UserRow key={u.login} u={u} />
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}
