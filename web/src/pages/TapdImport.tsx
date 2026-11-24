// Importing requirements from TAPD: the project's owner connects a TAPD workspace (credentials are sealed on the server
// and never shown again), members import its open stories.
import { Settings2 } from "lucide-react";
import { useState } from "react";
import { useDisconnectTapd, useImportTapd, usePutTapd, useTapd } from "@/api/digest";
import type { Role } from "@/api/types";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

function ConnectDialog({ projectId, open, onOpenChange }: { projectId: string; open: boolean; onOpenChange: (o: boolean) => void }) {
  const q = useTapd(projectId);
  const put = usePutTapd(projectId);
  const off = useDisconnectTapd(projectId);
  const cur = q.data;
  const [workspaceId, setWorkspaceId] = useState(cur?.workspaceId ?? "");
  const [mode, setMode] = useState<"account" | "token">(cur?.hasToken ? "token" : "account");
  const [user, setUser] = useState(cur?.user ?? "");
  const [password, setPassword] = useState("");
  const [token, setToken] = useState("");
  const [baseUrl, setBaseUrl] = useState(cur?.baseUrl ?? "");
  const [statuses, setStatuses] = useState(cur?.openStatuses ?? "");
  const connected = !!cur?.connected;
  const keep = connected ? "Leave empty to keep the saved one" : "";
  const ready = /^\d+$/.test(workspaceId.trim()) && (mode === "token" ? token.trim() !== "" || (connected && cur?.hasToken) : user.trim() !== "" && (password !== "" || (connected && !!cur?.hasPassword)));
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{connected ? "TAPD settings" : "Connect TAPD"}</DialogTitle>
          <DialogDescription>
            Requirements are read from one TAPD project. The password or token is stored encrypted on the server and cannot be read back.
          </DialogDescription>
        </DialogHeader>
        <form
          className="space-y-3"
          onSubmit={(e) => {
            e.preventDefault();
            put.mutate(
              { workspaceId: workspaceId.trim(), baseUrl: baseUrl.trim(), openStatuses: statuses.trim(), ...(mode === "token" ? { token: token.trim() } : { user: user.trim(), password }) },
              { onSuccess: () => { setPassword(""); setToken(""); onOpenChange(false); } },
            );
          }}
        >
          <div className="space-y-1">
            <Label htmlFor="tapd-ws">Workspace id (project id)</Label>
            <Input id="tapd-ws" inputMode="numeric" value={workspaceId} onChange={(e) => setWorkspaceId(e.target.value)} placeholder="10104801" autoComplete="off" />
          </div>
          <div role="group" aria-label="Sign in with" className="flex gap-2">
            <Button type="button" size="sm" variant={mode === "account" ? "secondary" : "ghost"} aria-pressed={mode === "account"} onClick={() => setMode("account")}>API account</Button>
            <Button type="button" size="sm" variant={mode === "token" ? "secondary" : "ghost"} aria-pressed={mode === "token"} onClick={() => setMode("token")}>Access token</Button>
          </div>
          {mode === "account" ? (
            <>
              <div className="space-y-1">
                <Label htmlFor="tapd-user">API user</Label>
                <Input id="tapd-user" value={user} onChange={(e) => setUser(e.target.value)} autoComplete="off" />
              </div>
              <div className="space-y-1">
                <Label htmlFor="tapd-pass">API password</Label>
                <Input id="tapd-pass" type="password" value={password} onChange={(e) => setPassword(e.target.value)} placeholder={keep} autoComplete="new-password" />
              </div>
            </>
          ) : (
            <div className="space-y-1">
              <Label htmlFor="tapd-token">Access token</Label>
              <Input id="tapd-token" type="password" value={token} onChange={(e) => setToken(e.target.value)} placeholder={keep} autoComplete="new-password" />
            </div>
          )}
          <div className="space-y-1">
            <Label htmlFor="tapd-base">Base URL (private deployments only)</Label>
            <Input id="tapd-base" value={baseUrl} onChange={(e) => setBaseUrl(e.target.value)} placeholder="https://api.tapd.cn" autoComplete="off" />
          </div>
          <div className="space-y-1">
            <Label htmlFor="tapd-open">Open statuses (optional)</Label>
            <Input id="tapd-open" value={statuses} onChange={(e) => setStatuses(e.target.value)} placeholder="planning, developing" autoComplete="off" />
            <p className="text-xs text-muted-foreground">
              Normally a story is open unless its status is a final one of its workflow, which TAPD tells us. If TAPD does not answer that for this account, or you want your own rule, list the English status names that count as open here: the list wins.
            </p>
          </div>
          <DialogFooter className="gap-2 sm:justify-between">
            {connected ? (
              <Button type="button" variant="ghost" disabled={off.isPending} onClick={() => off.mutate(undefined, { onSuccess: () => onOpenChange(false) })}>Disconnect</Button>
            ) : (
              <span />
            )}
            <Button type="submit" disabled={!ready || put.isPending}>{connected ? "Save" : "Connect"}</Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

/** The TAPD buttons of the requirements toolbar and the line that reports an import. */
export function TapdImport({ projectId, role }: { projectId: string; role: Role }) {
  const q = useTapd(projectId);
  const run = useImportTapd(projectId);
  const [dialog, setDialog] = useState(false);
  if (!q.data || role === "viewer") return null;
  const t = q.data;
  const owner = role === "owner";
  if (!t.connected && (!owner || !t.available)) return null;
  const r = run.data;
  return (
    <>
      {t.connected ? (
        <>
          <Button variant="outline" size="sm" disabled={run.isPending} onClick={() => run.mutate()}>Import from TAPD</Button>
          {owner ? (
            <Button variant="ghost" size="icon" aria-label="TAPD settings" onClick={() => setDialog(true)}>
              <Settings2 className="size-4" aria-hidden />
            </Button>
          ) : null}
        </>
      ) : (
        <Button variant="outline" size="sm" onClick={() => setDialog(true)}>Connect TAPD</Button>
      )}
      {dialog ? <ConnectDialog key={`${t.workspaceId}${t.user}${t.baseUrl}${t.openStatuses}`} projectId={projectId} open onOpenChange={setDialog} /> : null}
      {r ? (
        <p role="status" className="w-full text-right text-sm text-muted-foreground" data-testid="tapd-result">
          Imported {r.imported} TAPD story(ies), skipped {r.skipped} already known. Open stories were decided {r.source === "filter" ? "by your list of open statuses" : "from the final statuses of the TAPD workflows"}
          {r.truncated ? "; there are more than one import takes, run it again after working through these" : ""}.
        </p>
      ) : null}
    </>
  );
}
