// Queries of the notification rules, channels and personal preferences (see app/product/notifications.go).
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api } from "./client";
import { toastError } from "./queries";
import type { MyNotifications, NotifyChannel, NotifyLogEntry, NotifyRules, NotifyTestResult, NotifyTestResults, NotifyView } from "./types";

const base = (pid: string) => `/projects/${pid}/notifications`;
const channels = (pid: string) => `/projects/${pid}/notification-channels`;
const key = (pid: string) => ["notifications", pid] as const;

export const useNotifications = (pid: string) => useQuery({ queryKey: key(pid), queryFn: () => api.get<NotifyView>(base(pid)), retry: false });

export const useNotificationLog = (pid: string, enabled: boolean) =>
  useQuery({ queryKey: [...key(pid), "log"] as const, queryFn: () => api.get<NotifyLogEntry[]>(`${base(pid)}/log`), enabled, retry: false, refetchInterval: 10_000 });

export type RulesInput = Pick<NotifyRules, "events" | "dedupeMinutes" | "digest">;

export function useSaveRules(pid: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (r: RulesInput) => api.put<NotifyView>(base(pid), r),
    onSuccess: (v) => qc.setQueryData(key(pid), v),
    onError: toastError,
  });
}

export interface ChannelInput {
  kind?: string;
  name?: string;
  url?: string;
  secret?: string;
  clearSecret?: boolean;
  enabled?: boolean;
}

export function useAddChannel(pid: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (c: ChannelInput) => api.post<NotifyChannel>(channels(pid), c),
    onSuccess: () => void qc.invalidateQueries({ queryKey: key(pid) }),
    onError: toastError,
  });
}

export function useUpdateChannel(pid: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, ...c }: ChannelInput & { id: string }) => api.put<NotifyChannel>(`${channels(pid)}/${id}`, c),
    onSuccess: () => void qc.invalidateQueries({ queryKey: key(pid) }),
    onError: toastError,
  });
}

export function useDeleteChannel(pid: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.del(`${channels(pid)}/${id}`),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: key(pid) });
      toast.success("Channel removed");
    },
    onError: toastError,
  });
}

export function useTestChannel(pid: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.post<NotifyTestResult>(`${channels(pid)}/${id}/test`),
    onSettled: () => void qc.invalidateQueries({ queryKey: [...key(pid), "log"] }),
  });
}

const mine = ["me-notifications"] as const;
export const useMyNotifications = () => useQuery({ queryKey: mine, queryFn: () => api.get<MyNotifications>("/me/notifications"), retry: false });

export interface MyInput {
  events: Record<string, string[]>;
  quietStart: string;
  quietEnd: string;
  timezone: string;
  digest: boolean;
  webhook?: { url?: string; secret?: string; clear?: boolean };
}

export function useSaveMyNotifications() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (b: MyInput) => api.put<MyNotifications>("/me/notifications", b),
    onSuccess: (v) => {
      qc.setQueryData(mine, v);
      toast.success("Notification settings saved");
    },
    onError: toastError,
  });
}

export function useTestMine() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api.post<NotifyTestResults>("/me/notifications/test"),
    onSettled: () => void qc.invalidateQueries({ queryKey: mine }),
  });
}
