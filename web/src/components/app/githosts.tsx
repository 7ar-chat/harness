// Code hosts besides GitHub: the user's tokens and SSH keys (Profile, "Git access") and the administrator's list of
// allowed hosts (Admin, "Git hosts").
import { AlertTriangle, Copy, KeyRound, Plus, Trash2 } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";
import {
  useDeleteGitHost,
  useDeleteGitSSHKey,
  useDeleteGitToken,
  useGitHosts,
  useMakeGitSSHKey,
  useSaveGitHost,
  useScanGitHostKeys,
  useSetGitToken,
} from "@/api/queries";
import type { GitHost } from "@/api/generated";
import { Spinner } from "@/components/app/common";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";

const KIND_LABEL: Record<string, string> = { gitlab: "GitLab", gitea: "Gitea / Forgejo", git: "Plain git" };

function copy(text: string) {
  void navigator.clipboard?.writeText(text).then(() => toast.success("Copied"), () => toast.error("Could not copy"));
}

// ---- Profile: the user's own access ----

function HostAccess({ h }: { h: GitHost }) {
  const setToken = useSetGitToken();
  const delToken = useDeleteGitToken();
  const makeKey = useMakeGitSSHKey();
  const delKey = useDeleteGitSSHKey();
  const [token, setTokenText] = useState("");
  const [username, setUsername] = useState("");
  const cred = h.credential;
  const sshReady = h.knownHosts.length > 0;
  return (
    <li className="space-y-3 rounded-lg border p-3" aria-label={h.name}>
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-medium">{h.name}</span>
        <Badge variant="outline">{KIND_LABEL[h.kind] ?? h.kind}</Badge>
        {!h.api ? <Badge variant="secondary">no pull request API</Badge> : null}
      </div>

      <div className="space-y-2">
        <Label htmlFor={`tok-${h.name}`}>Personal access token</Label>
        {cred.hasToken ? (
          <div className="flex flex-wrap items-center gap-2 text-sm">
            <span className="text-muted-foreground">A token is saved{cred.username ? ` for ${cred.username}` : ""}. It is never shown again.</span>
            <Button size="sm" variant="outline" disabled={delToken.isPending} onClick={() => delToken.mutate(h.name)}>
              <Trash2 className="size-4" aria-hidden /> Remove token
            </Button>
          </div>
        ) : null}
        <div className="flex flex-wrap gap-2">
          <Input id={`tok-${h.name}`} type="password" autoComplete="off" placeholder={cred.hasToken ? "Replace the token" : "Paste a token"} value={token} onChange={(e) => setTokenText(e.target.value)} className="min-w-48 flex-1" />
          <Input aria-label={`User name on ${h.name}`} placeholder="user name (optional)" value={username} onChange={(e) => setUsername(e.target.value)} className="w-44" />
          <Button
            size="sm"
            disabled={!token.trim() || setToken.isPending}
            onClick={() => setToken.mutate({ host: h.name, token: token.trim(), username: username.trim() || undefined }, { onSuccess: () => setTokenText("") })}
          >
            Save token
          </Button>
        </div>
        <p className="text-xs text-muted-foreground">Needs the api scope (GitLab) or the repository and issue scopes (Gitea). It is sealed on the server and never returned.</p>
      </div>

      <div className="space-y-2">
        <Label>SSH key</Label>
        {cred.hasSshKey ? (
          <div className="space-y-2">
            <p className="text-xs text-muted-foreground">Add this public key to your account on {h.name}. The private half stays on the server.</p>
            <div className="flex items-start gap-2">
              <Textarea readOnly aria-label={`Public key for ${h.name}`} value={cred.publicKey ?? ""} className="min-h-16 font-mono text-xs" />
              <Button size="icon" variant="outline" aria-label="Copy public key" onClick={() => copy(cred.publicKey ?? "")}>
                <Copy className="size-4" aria-hidden />
              </Button>
            </div>
            {cred.fingerprint ? <p className="font-mono text-xs text-muted-foreground">{cred.fingerprint}</p> : null}
            <Button size="sm" variant="outline" disabled={delKey.isPending} onClick={() => delKey.mutate(h.name)}>
              <Trash2 className="size-4" aria-hidden /> Remove key
            </Button>
          </div>
        ) : (
          <div className="flex flex-wrap items-center gap-2">
            <Button size="sm" variant="outline" disabled={makeKey.isPending} onClick={() => makeKey.mutate(h.name)}>
              <KeyRound className="size-4" aria-hidden /> Generate an SSH key
            </Button>
            {!sshReady ? <span className="text-xs text-muted-foreground">SSH is used once an administrator registers the host's SSH keys; until then the token is used.</span> : null}
          </div>
        )}
      </div>
    </li>
  );
}

/** Profile → "Git access": a token and an SSH key per registered host. Hidden when the server has the feature off. */
export function GitAccess() {
  const q = useGitHosts();
  if (q.isLoading) return <Spinner />;
  if (!q.data) return null;
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <KeyRound className="size-4" aria-hidden /> Git access
        </CardTitle>
        <CardDescription>Your tokens and SSH keys for GitLab, Gitea and other git hosts. Your agent clones, pushes and opens merge requests with them.</CardDescription>
      </CardHeader>
      <CardContent>
        {q.data.hosts.length === 0 ? (
          <p className="text-sm text-muted-foreground">No git host besides GitHub is registered. {q.data.admin ? "Add one under Admin, Git hosts." : "Ask an administrator to add the hosts you work with."}</p>
        ) : (
          <ul className="space-y-3">
            {q.data.hosts.map((h) => (
              <HostAccess key={h.name} h={h} />
            ))}
          </ul>
        )}
      </CardContent>
    </Card>
  );
}

// ---- Admin: the list of allowed hosts ----

function HostForm({ initial, onDone }: { initial?: GitHost; onDone: () => void }) {
  const save = useSaveGitHost();
  const scan = useScanGitHostKeys();
  const [name, setName] = useState(initial?.name ?? "");
  const [kind, setKind] = useState(initial?.kind ?? "gitlab");
  const [baseUrl, setBaseUrl] = useState(initial?.baseUrl ?? "");
  const [sshHost, setSshHost] = useState(initial?.sshHost ?? "");
  const [sshPort, setSshPort] = useState(initial?.sshPort ? String(initial.sshPort) : "");
  const [keys, setKeys] = useState((initial?.knownHosts ?? []).join("\n"));
  const [svc, setSvc] = useState("");
  const [svcUser, setSvcUser] = useState("");
  const editing = !!initial;
  const submit = (clearServiceToken = false) =>
    save.mutate(
      {
        name: name.trim(),
        kind,
        baseUrl: baseUrl.trim() || undefined,
        sshHost: sshHost.trim() || undefined,
        sshPort: sshPort.trim() ? Number(sshPort) : undefined,
        knownHosts: keys.split("\n").map((l) => l.trim()).filter(Boolean),
        serviceToken: svc.trim() || undefined,
        serviceUsername: svcUser.trim() || undefined,
        clearServiceToken: clearServiceToken || undefined,
      },
      { onSuccess: onDone },
    );
  return (
    <div className="space-y-3 rounded-lg border bg-card p-3">
      <div className="grid gap-3 sm:grid-cols-2">
        <div className="space-y-1">
          <Label htmlFor="gh-name">Host name</Label>
          <Input id="gh-name" placeholder="gitlab.example.com" value={name} disabled={editing} onChange={(e) => setName(e.target.value)} />
        </div>
        <div className="space-y-1">
          <Label htmlFor="gh-kind">Kind</Label>
          <select id="gh-kind" value={kind} onChange={(e) => setKind(e.target.value)} className="h-9 w-full rounded-md border bg-background px-2 text-sm">
            <option value="gitlab">GitLab</option>
            <option value="gitea">Gitea / Forgejo</option>
            <option value="git">Plain git (HTTPS or SSH)</option>
          </select>
        </div>
        <div className="space-y-1 sm:col-span-2">
          <Label htmlFor="gh-url">Web address</Label>
          <Input id="gh-url" placeholder={`https://${name || "gitlab.example.com"}`} value={baseUrl} onChange={(e) => setBaseUrl(e.target.value)} />
        </div>
        <div className="space-y-1">
          <Label htmlFor="gh-ssh">SSH host (if different)</Label>
          <Input id="gh-ssh" value={sshHost} onChange={(e) => setSshHost(e.target.value)} />
        </div>
        <div className="space-y-1">
          <Label htmlFor="gh-port">SSH port</Label>
          <Input id="gh-port" inputMode="numeric" placeholder="22" value={sshPort} onChange={(e) => setSshPort(e.target.value)} />
        </div>
      </div>
      <div className="space-y-1">
        <div className="flex items-center justify-between">
          <Label htmlFor="gh-keys">SSH host keys</Label>
          {editing ? (
            <Button
              size="sm"
              variant="outline"
              disabled={scan.isPending}
              onClick={() => scan.mutate(name, { onSuccess: (r) => setKeys(r.knownHosts.join("\n")) })}
            >
              Read keys from the host
            </Button>
          ) : null}
        </div>
        <Textarea id="gh-keys" value={keys} onChange={(e) => setKeys(e.target.value)} placeholder="gitlab.example.com ssh-ed25519 AAAA..." className="min-h-20 font-mono text-xs" />
        <p className="text-xs text-muted-foreground">SSH connections accept only these keys. Compare them with what the host publishes before saving: reading them from the host trusts the network between here and there.</p>
      </div>
      <div className="grid gap-3 sm:grid-cols-2">
        <div className="space-y-1">
          <Label htmlFor="gh-svc">Shared service token (optional)</Label>
          <Input id="gh-svc" type="password" autoComplete="off" placeholder={initial?.hasServiceToken ? "Replace the token" : "Used by people with no token of their own"} value={svc} onChange={(e) => setSvc(e.target.value)} />
        </div>
        <div className="space-y-1">
          <Label htmlFor="gh-svc-user">Service user name</Label>
          <Input id="gh-svc-user" value={svcUser} onChange={(e) => setSvcUser(e.target.value)} />
        </div>
      </div>
      <div className="flex flex-wrap gap-2">
        <Button size="sm" disabled={!name.trim() || save.isPending} onClick={() => submit()}>
          {editing ? "Save" : "Add host"}
        </Button>
        {initial?.hasServiceToken ? (
          <Button size="sm" variant="outline" onClick={() => submit(true)}>
            Remove the service token
          </Button>
        ) : null}
        <Button size="sm" variant="ghost" onClick={onDone}>
          Cancel
        </Button>
      </div>
    </div>
  );
}

function AdminHostRow({ h }: { h: GitHost }) {
  const del = useDeleteGitHost();
  const [editing, setEditing] = useState(false);
  if (editing) return <HostForm initial={h} onDone={() => setEditing(false)} />;
  return (
    <li className="space-y-2 rounded-lg border bg-card p-3" aria-label={h.name}>
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-medium">{h.name}</span>
        <Badge variant="outline">{KIND_LABEL[h.kind] ?? h.kind}</Badge>
        <Badge variant="secondary">{h.knownHosts.length ? `${h.knownHosts.length} SSH key(s)` : "no SSH keys"}</Badge>
        {h.hasServiceToken ? <Badge variant="secondary">service token</Badge> : null}
        <div className="ml-auto flex gap-2">
          <Button size="sm" variant="outline" onClick={() => setEditing(true)}>
            Edit
          </Button>
          <Button size="sm" variant="outline" disabled={del.isPending} onClick={() => del.mutate(h.name)}>
            <Trash2 className="size-4" aria-hidden /> Remove
          </Button>
        </div>
      </div>
      <p className="text-xs text-muted-foreground">
        The sandbox needs to reach: <span className="font-mono">{h.egress.join(", ")}</span>
      </p>
      {h.egressBlocked.length ? (
        <p role="alert" className="flex items-center gap-1 text-xs text-amber-700 dark:text-amber-400">
          <AlertTriangle className="size-3.5" aria-hidden /> SANDBOX_EGRESS_ALLOWLIST does not include {h.egressBlocked.join(", ")}; clones and pushes will fail until it does.
        </p>
      ) : null}
    </li>
  );
}

/** Admin → "Git hosts": the hosts the server may reach (git host trust), with their SSH host keys. */
export function GitHostsAdmin() {
  const q = useGitHosts();
  const [adding, setAdding] = useState(false);
  if (q.isLoading) return <Spinner />;
  if (!q.data) return <p className="text-sm text-muted-foreground">Git hosts are not enabled on this server (they need team mode and HARNESS_SECRET_KEY).</p>;
  return (
    <div className="space-y-3">
      <p className="text-sm text-muted-foreground">
        Only the hosts listed here can be cloned from, pushed to or called through their API. Everyone else keeps a token or an SSH key per host under Settings, Git access.
      </p>
      {q.data.hosts.length === 0 && !adding ? <p className="text-sm text-muted-foreground">No host is registered besides GitHub.</p> : null}
      <ul className="space-y-2">
        {q.data.hosts.map((h) => (
          <AdminHostRow key={h.name} h={h} />
        ))}
      </ul>
      {adding ? (
        <HostForm onDone={() => setAdding(false)} />
      ) : (
        <Button size="sm" onClick={() => setAdding(true)}>
          <Plus className="size-4" aria-hidden /> Add a git host
        </Button>
      )}
    </div>
  );
}
