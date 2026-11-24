// The weekly digest of a project (GET/PUT .../digest, POST .../digest/send) and the TAPD connection of its requirements.
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api, isUnsupported } from "./client";
import { toastError } from "./queries";
import type { DigestSendResult, DigestView, RequirementImport, TapdStatus } from "./types";

export type { DigestSendResult, DigestView, TapdStatus };

export const digestKey = (pid: string) => ["digest", pid] as const;

/** The digest setting and history, or null when this server keeps none (404/501). */
export const useDigest = (pid: string) =>
  useQuery({
    queryKey: digestKey(pid),
    queryFn: async () => {
      try {
        return await api.get<DigestView>(`/projects/${pid}/digest`);
      } catch (e) {
        if (isUnsupported(e)) return null;
        throw e;
      }
    },
    retry: false,
  });

export interface DigestSettingInput {
  enabled: boolean;
  weekday: number;
  hour: number;
  timezone: string;
}

export const usePutDigest = (pid: string) => {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (b: DigestSettingInput) => api.put<DigestView>(`/projects/${pid}/digest`, b),
    onSuccess: (v) => {
      qc.setQueryData(digestKey(pid), v);
      toast.success(v.setting.enabled ? "Weekly digest saved" : "Weekly digest is off");
    },
    onError: toastError,
  });
};

export const useSendDigest = (pid: string) => {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api.post<DigestSendResult>(`/projects/${pid}/digest/send`),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: digestKey(pid) });
      toast.success("Digest sent");
    },
    onError: toastError,
  });
};

export const tapdKey = (pid: string) => ["tapd", pid] as const;

/** The TAPD connection of the project, or null when this server has none. */
export const useTapd = (pid: string) =>
  useQuery({
    queryKey: tapdKey(pid),
    queryFn: async () => {
      try {
        return await api.get<TapdStatus>(`/projects/${pid}/requirements/tapd`);
      } catch (e) {
        if (isUnsupported(e)) return null;
        throw e;
      }
    },
    retry: false,
  });

export interface TapdInput {
  workspaceId: string;
  baseUrl?: string;
  user?: string;
  password?: string;
  token?: string;
  openStatuses?: string;
}

export const usePutTapd = (pid: string) => {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (b: TapdInput) => api.put<TapdStatus>(`/projects/${pid}/requirements/tapd`, b),
    onSuccess: (v) => {
      qc.setQueryData(tapdKey(pid), v);
      toast.success("TAPD connected");
    },
    onError: toastError,
  });
};

export const useDisconnectTapd = (pid: string) => {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api.del(`/projects/${pid}/requirements/tapd`),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: tapdKey(pid) });
      toast.success("TAPD disconnected");
    },
    onError: toastError,
  });
};

export const useImportTapd = (pid: string) => {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api.post<RequirementImport>(`/projects/${pid}/requirements/import/tapd`, {}),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ["requirements", pid] }),
    onError: toastError,
  });
};
