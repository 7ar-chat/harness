// Where a page "is": the project and the search (tab, task, wf, run, req) it shows. The shell keeps several tabs mounted at once, so a page
// cannot read the address (that is only the active tab's); it reads its own place from here, and falls back to the router outside the shell.
import { useParams, useSearch } from "@tanstack/react-router";
import { createContext, useContext } from "react";

export interface ProjectSearch {
  tab?: string;
  task?: string;
  wf?: string;
  run?: string;
  req?: string;
}
export interface Place {
  projectId?: string;
  threadId?: string;
  search: ProjectSearch;
}

export const PlaceCtx = createContext<Place | null>(null);

export function useProjectPlace(): { projectId: string; search: ProjectSearch } {
  const place = useContext(PlaceCtx);
  const routed = useSearch({ strict: false }) as ProjectSearch;
  const params = useParams({ strict: false }) as { projectId?: string };
  return place ? { projectId: place.projectId ?? "", search: place.search } : { projectId: params.projectId ?? "", search: routed };
}

export const useProjectSearch = () => useProjectPlace().search;

/** The place of an href such as /p/ab12?tab=tasks&task=t1. */
export function placeOf(href: string): Place {
  const [path = "", query = ""] = href.split("?");
  const q = new URLSearchParams(query);
  const get = (k: string) => q.get(k) || undefined;
  return {
    projectId: /^\/p\/([^/]+)$/.exec(path)?.[1],
    threadId: /^\/t\/([^/]+)$/.exec(path)?.[1],
    search: { tab: get("tab"), task: get("task"), wf: get("wf"), run: get("run"), req: get("req") },
  };
}
