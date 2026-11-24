// "Try it with a sample": a small project from a template, a first session in it and a ready prompt, so an agent is seen working at once.
import { useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useCallback, useState } from "react";
import { api } from "@/api/client";
import { qk, toastError } from "@/api/queries";
import type { Project, Thread } from "@/api/types";

export const SAMPLE_PROMPT = "Look at this project and tell me in three short lines what it is. Then add a friendly welcome heading to the main page and show me the change.";
const PREFERRED = ["static-html", "react-vite"];

export function useTrySample() {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [busy, setBusy] = useState(false);
  const start = useCallback(async () => {
    if (busy) return;
    setBusy(true);
    try {
      const templates = await api.get<{ id: string }[]>("/templates").catch(() => []);
      const template = PREFERRED.map((id) => templates.find((t) => t.id === id)).find(Boolean)?.id ?? templates[0]?.id;
      const project = await api.post<Project>("/projects", { name: "Sample project", description: "A small project to try 7ar on.", ...(template ? { template } : {}) });
      const thread = await api.post<Thread>("/threads", { title: "Try 7ar", projectId: project.id });
      await api.post(`/threads/${thread.id}/runs`, { message: SAMPLE_PROMPT, interactive: true });
      void qc.invalidateQueries({ queryKey: qk.overview });
      void qc.invalidateQueries({ queryKey: qk.me });
      void navigate({ to: "/t/$threadId", params: { threadId: thread.id } });
    } catch (e) {
      toastError(e);
    } finally {
      setBusy(false);
    }
  }, [busy, navigate, qc]);
  return { start, busy };
}
