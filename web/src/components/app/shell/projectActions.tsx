// The project-level actions that used to sit in every project page's header: new session, import AGENTS.md, export MEMORY.md.
// They live in the explorer's project menu and the command palette now.
import { toast } from "sonner";
import { ApiError, canWrite, downloadText } from "@/api/client";
import { toastError, useImportAgents, useOverview } from "@/api/queries";
import { useDialogs } from "../dialogs";
import { useGithubConnector } from "../github-import";

export function useProjectActions(projectId?: string) {
  const project = useOverview().data?.projects.find((p) => p.id === projectId);
  const { newSession } = useDialogs();
  const importAgents = useImportAgents(projectId ?? "");
  const { ready: ghReady } = useGithubConnector();
  if (!project) return null;
  const writable = canWrite(project.role);
  return {
    project,
    canWrite: writable,
    newSession: () => newSession(project.id),
    canImport: writable && ghReady,
    importHint: ghReady ? "Import AGENTS.md from the repository as a pinned memory" : "Connect GitHub in Connectors to import AGENTS.md",
    importAgents: () =>
      importAgents.mutate(undefined, {
        onSuccess: (e) => toast.success(`Imported ${e.title || "AGENTS.md"} as a pinned memory`),
        onError: (e) => (e instanceof ApiError && e.status === 404 ? toast.info("No AGENTS.md found in the repository, or GitHub is not connected.") : toastError(e)),
      }),
    exportMemory: () => void downloadText(`/projects/${project.id}/memory/export`, "MEMORY.md").catch(toastError),
  };
}
