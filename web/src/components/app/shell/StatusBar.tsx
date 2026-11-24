// The status bar: repository and branch, running sessions, spend against the cap, sandbox, user.
import { CircleHelp, PanelBottom, PanelLeft } from "lucide-react";
import { useMe, useOverview } from "@/api/queries";
import { useUsage } from "@/api/schedulesUsage";
import { useEnvironments } from "@/chat/environment";
import { formatUsd } from "@/lib/money";
import { cn } from "@/lib/utils";
import { useShell } from "./context";

const usd = formatUsd;
const repoName = (url?: string) => (url ? url.replace(/^(https?:\/\/|ssh:\/\/|git@)/, "").replace(/^[^/:]+[/:]/, "").replace(/\.git$/, "") : "");

const Btn = ({ pressed, className, ...p }: { pressed?: boolean } & React.ButtonHTMLAttributes<HTMLButtonElement>) => (
  <button type="button" aria-pressed={pressed} className={cn("inline-flex h-full items-center gap-1 px-1.5 outline-none hover:bg-black/15 focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-statusbar-foreground", pressed && "bg-black/15", className)} {...p} />
);

export function StatusBar() {
  const { projectId, explorer, setExplorer, panel, setPanel, needsCount, tabs } = useShell();
  const overview = useOverview().data;
  const me = useMe().data?.user;
  const envs = useEnvironments().data;
  const usage = useUsage({ projectId, range: "month", group: "day" }).data;
  const project = overview?.projects.find((p) => p.id === projectId);
  const all = [...(overview?.personal ?? []), ...(overview?.projects ?? []).flatMap((p) => p.threads)];
  const running = all.filter((t) => t.status === "running" || t.status === "queued").length;
  const cap = usage?.cap?.monthlyUsd ?? 0;
  const repo = repoName(project?.repoUrl) || project?.name || "personal";
  return (
    <footer className="flex h-6 shrink-0 items-center gap-3 bg-statusbar pr-3 font-mono text-xs font-medium text-statusbar-foreground" aria-label="Status bar">
      <Btn pressed={explorer} onClick={() => setExplorer((v) => !v)} aria-label="Toggle explorer (Ctrl/Cmd+B)" title="Toggle explorer (Ctrl/Cmd+B)">
        <PanelLeft className="size-3.5" />
      </Btn>
      <span className="min-w-0 truncate">
        {repo}
        {project?.defaultBranch ? ` · ${project.defaultBranch}` : ""}
      </span>
      <span className="hidden whitespace-nowrap sm:inline">{running} running</span>
      {usage ? (
        <span className="hidden whitespace-nowrap sm:inline" title="This month">
          {usd(usage.total.costUsd)}
          {cap ? ` / ${usd(cap)}` : ""}
        </span>
      ) : null}
      <span className="ml-auto hidden whitespace-nowrap md:inline">sandbox: {envs?.default ?? "default"}</span>
      <span className="hidden whitespace-nowrap md:inline">{me?.login}</span>
      <Btn onClick={() => tabs.open("/help")} aria-label="Help" title="Help and getting started" className="ml-auto md:ml-0">
        <CircleHelp className="size-3.5" />
        <span className="hidden sm:inline">Help</span>
      </Btn>
      <Btn pressed={panel} onClick={() => setPanel((v) => !v)} aria-label={needsCount ? `Panel ${needsCount} waiting for you` : "Panel"} title="Toggle panel (Ctrl/Cmd+J)" className="md:ml-0">
        <PanelBottom className="size-3.5" />
        <span className="hidden sm:inline">Panel</span>
        {needsCount ? (
          <span className="rounded-full bg-statusbar-foreground px-1.5 text-[10px] font-semibold text-statusbar" aria-label={`${needsCount} waiting for you`}>
            {needsCount}
          </span>
        ) : null}
      </Btn>
    </footer>
  );
}
