import { AlertTriangle, Loader2 } from "lucide-react";
import type { ReactNode } from "react";
import { ApiError } from "@/api/client";
import { useUserDirectory } from "@/api/queries";
import type { Role, ThreadStatus } from "@/api/types";
import { Avatar, AvatarFallback, AvatarImage } from "@/components/ui/avatar";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";

export function UserAvatar({ login, name, url, className }: { login: string; name?: string; url?: string; className?: string }) {
  const known = useUserDirectory().get(login);
  url = url || known?.avatarUrl || undefined;
  const label = name || known?.name || login;
  return (
    <Avatar className={cn("size-6 text-[10px]", className)} title={label}>
      {url ? <AvatarImage src={url} alt={label} /> : null}
      <AvatarFallback className="bg-primary/15">{label.slice(0, 2).toUpperCase()}</AvatarFallback>
    </Avatar>
  );
}

export const STATUS_LABEL: Record<ThreadStatus, string> = {
  idle: "Idle",
  queued: "Queued",
  running: "Running",
  waiting_approval: "Needs approval",
  waiting_input: "Needs answer",
  failed: "Failed",
};

const STATUS_COLOR: Record<ThreadStatus, string> = {
  idle: "bg-muted-foreground/40",
  queued: "bg-muted-foreground",
  running: "bg-success animate-pulse",
  waiting_approval: "bg-warning",
  waiting_input: "bg-warning",
  failed: "bg-destructive",
};

export const needsYou = (s: ThreadStatus) => s === "waiting_approval" || s === "waiting_input" || s === "failed";
export const isActive = (s: ThreadStatus) => s !== "idle";

export function StatusDot({ status, className }: { status: ThreadStatus; className?: string }) {
  return <span role="img" aria-label={STATUS_LABEL[status]} title={STATUS_LABEL[status]} className={cn("inline-block size-2 shrink-0 rounded-full", STATUS_COLOR[status], className)} />;
}

export function StatusBadge({ status }: { status: ThreadStatus }) {
  const variant = status === "failed" ? "destructive" : needsYou(status) ? "default" : "secondary";
  return (
    <Badge variant={variant} className="gap-1.5 font-normal">
      <StatusDot status={status} className="size-1.5" />
      {STATUS_LABEL[status]}
    </Badge>
  );
}

export const RoleBadge = ({ role }: { role: Role }) => (
  <Badge variant="outline" className="font-normal capitalize">
    {role}
  </Badge>
);

export function Empty({ icon, title, children }: { icon?: ReactNode; title: string; children?: ReactNode }) {
  return (
    <div className="flex flex-col items-center justify-center gap-3 p-8 text-center text-muted-foreground">
      {icon}
      <div className="text-sm font-medium text-foreground">{title}</div>
      {children ? <div className="max-w-sm text-sm">{children}</div> : null}
    </div>
  );
}

export function ErrorState({ error, retry }: { error: unknown; retry?: () => void }) {
  const status = error instanceof ApiError ? error.status : 0;
  const title = status === 403 ? "You do not have access to this" : status === 404 ? "Not found" : "Something went wrong";
  const msg = status === 404 ? "It may have been deleted, or you may not be a member of its project." : error instanceof Error ? error.message : String(error);
  return (
    <Empty icon={<AlertTriangle className="size-8" />} title={title}>
      <p>{msg}</p>
      {retry && status !== 403 && status !== 404 ? (
        <Button variant="outline" size="sm" className="mt-3" onClick={retry}>
          Try again
        </Button>
      ) : null}
    </Empty>
  );
}

export const Spinner = ({ className }: { className?: string }) => <Loader2 className={cn("size-4 animate-spin", className)} aria-label="Loading" />;

export function ListSkeleton({ rows = 4 }: { rows?: number }) {
  return (
    <div className="space-y-2 p-4" aria-busy="true">
      {Array.from({ length: rows }, (_, i) => (
        <Skeleton key={i} className="h-10 w-full" />
      ))}
    </div>
  );
}
