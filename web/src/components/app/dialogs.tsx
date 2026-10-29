// App-wide dialogs (new session, new project) opened from the sidebar, palette and pages.
import { useNavigate } from "@tanstack/react-router";
import { createContext, useContext, useState, type ReactNode } from "react";
import { useCreateProject, useCreateThread, useMe } from "@/api/queries";
import { canWrite } from "@/api/client";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";

interface Ctx {
  newSession: (projectId?: string) => void;
  newProject: () => void;
  openPalette: () => void;
}
const DialogsCtx = createContext<Ctx>({ newSession: () => {}, newProject: () => {}, openPalette: () => {} });
export const useDialogs = () => useContext(DialogsCtx);

const PERSONAL = "__personal";

export function DialogsProvider({ children, openPalette }: { children: ReactNode; openPalette: () => void }) {
  const [session, setSession] = useState<{ open: boolean; project: string }>({ open: false, project: PERSONAL });
  const [project, setProject] = useState(false);
  return (
    <DialogsCtx.Provider value={{ newSession: (p) => setSession({ open: true, project: p ?? PERSONAL }), newProject: () => setProject(true), openPalette }}>
      {children}
      <NewSessionDialog open={session.open} project={session.project} onClose={() => setSession((s) => ({ ...s, open: false }))} />
      <NewProjectDialog open={project} onClose={() => setProject(false)} />
    </DialogsCtx.Provider>
  );
}

function NewSessionDialog({ open, project, onClose }: { open: boolean; project: string; onClose: () => void }) {
  const projects = (useMe().data?.projects ?? []).filter((p) => canWrite(p.role));
  const [title, setTitle] = useState("");
  const [pid, setPid] = useState<string | null>(null);
  const create = useCreateThread();
  const navigate = useNavigate();
  const selected = pid ?? project;
  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <form
          onSubmit={(e) => {
            e.preventDefault();
            create.mutate(
              { title: title.trim() || "New session", projectId: selected === PERSONAL ? undefined : selected },
              {
                onSuccess: (t) => {
                  setTitle("");
                  setPid(null);
                  onClose();
                  void navigate({ to: "/t/$threadId", params: { threadId: t.id } });
                },
              },
            );
          }}
        >
          <DialogHeader>
            <DialogTitle>New session</DialogTitle>
            <DialogDescription>Each session is its own agent with its own chat and workspace.</DialogDescription>
          </DialogHeader>
          <div className="my-4 space-y-4">
            <div className="space-y-2">
              <Label htmlFor="ns-title">Title</Label>
              <Input id="ns-title" value={title} onChange={(e) => setTitle(e.target.value)} placeholder="What should the agent work on?" autoFocus />
            </div>
            <div className="space-y-2">
              <Label>Project</Label>
              <Select value={selected} onValueChange={setPid}>
                <SelectTrigger aria-label="Project">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value={PERSONAL}>Personal (only you)</SelectItem>
                  {projects.map((p) => (
                    <SelectItem key={p.id} value={p.id}>
                      {p.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" disabled={create.isPending}>
              Create
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function NewProjectDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const [f, setF] = useState({ name: "", description: "", repoUrl: "", defaultBranch: "main" });
  const create = useCreateProject();
  const navigate = useNavigate();
  const set = (k: keyof typeof f) => (e: { target: { value: string } }) => setF({ ...f, [k]: e.target.value });
  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <form
          onSubmit={(e) => {
            e.preventDefault();
            create.mutate(
              { name: f.name.trim(), description: f.description.trim(), repoUrl: f.repoUrl.trim(), defaultBranch: f.defaultBranch.trim() },
              {
                onSuccess: (p) => {
                  setF({ name: "", description: "", repoUrl: "", defaultBranch: "main" });
                  onClose();
                  void navigate({ to: "/p/$projectId", params: { projectId: p.id } });
                },
              },
            );
          }}
        >
          <DialogHeader>
            <DialogTitle>New project</DialogTitle>
            <DialogDescription>A project is a repository whose sessions, memory and members are shared.</DialogDescription>
          </DialogHeader>
          <div className="my-4 space-y-4">
            <div className="space-y-2">
              <Label htmlFor="np-name">Name</Label>
              <Input id="np-name" value={f.name} onChange={set("name")} required maxLength={100} autoFocus />
            </div>
            <div className="space-y-2">
              <Label htmlFor="np-desc">Description</Label>
              <Textarea id="np-desc" value={f.description} onChange={set("description")} rows={2} />
            </div>
            <div className="space-y-2">
              <Label htmlFor="np-repo">Repository URL</Label>
              <Input id="np-repo" value={f.repoUrl} onChange={set("repoUrl")} placeholder="https://github.com/org/repo" type="url" />
            </div>
            <div className="space-y-2">
              <Label htmlFor="np-branch">Default branch</Label>
              <Input id="np-branch" value={f.defaultBranch} onChange={set("defaultBranch")} />
            </div>
          </div>
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" disabled={create.isPending || !f.name.trim()}>
              Create project
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
