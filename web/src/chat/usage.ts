import type { ModelInfo } from "@/api/types";
import type { Usage } from "@/lib/agui";

const k = (n: number) => (n >= 1000 ? `${(n / 1000).toFixed(1)}k` : String(n));

export function formatUsage(u?: Usage, info?: ModelInfo): string {
  if (!u) return "";
  const inT = u.inputTokens ?? 0;
  const outT = u.outputTokens ?? 0;
  const parts = [`${k(inT)} in`, `${k(outT)} out`];
  if (inT && u.cachedInputTokens) parts.push(`${Math.round((u.cachedInputTokens * 100) / inT)}% cached`);
  const cost = u.costUsd ?? (info?.priced && info.inputPerToken !== undefined ? inT * info.inputPerToken + outT * (info.outputPerToken ?? 0) : undefined);
  if (cost) parts.push(`$${cost.toFixed(cost < 0.1 ? 3 : 2)}`);
  if (u.contextTokens && u.contextWindow) parts.push(`ctx ${Math.round((u.contextTokens * 100) / u.contextWindow)}%`);
  return parts.join(" · ");
}
