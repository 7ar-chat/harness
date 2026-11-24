// What the interface remembers for a person, kept on the server too (GET/PATCH /api/me/preferences) so it follows them across
// browsers. localStorage stays the fast cache and the offline fallback: the screen never waits for the server, and every failure here
// is silent. Each remembered thing carries `at` (ms since the epoch, when it last changed here or there); when the browser and the
// server disagree, the later `at` wins. A thing that was never changed on this browser has no `at`, so it never overwrites the server.
import { api } from "@/api/client";

export interface Stamped {
  /** When it last changed, ms since the epoch. */
  at?: number;
}
export interface PrefsDoc {
  values: Record<string, unknown>;
  updatedAt?: string;
}

const stampOf = (v: unknown): number => (v && typeof v === "object" && typeof (v as Stamped).at === "number" ? ((v as Stamped).at as number) : 0);

/** Which side holds the truth for one key: the value to use now and whether the server must be told. */
export function mergeKey<T>(local: T | undefined, server: T | undefined): { value: T | undefined; push: boolean; adopted: boolean } {
  if (server === undefined) return { value: local, push: local !== undefined && stampOf(local) > 0, adopted: false };
  if (local === undefined) return { value: server, push: false, adopted: true };
  const l = stampOf(local);
  const s = stampOf(server);
  if (l > s) return { value: local, push: true, adopted: false };
  return { value: server, push: false, adopted: s > l || JSON.stringify(local) !== JSON.stringify(server) };
}

/** Reads the person's document; null when the server has none to give (an older server, offline, signed out). */
export async function fetchPrefs(get: (path: string) => Promise<PrefsDoc> = (p) => api.get<PrefsDoc>(p)): Promise<PrefsDoc | null> {
  try {
    const d = await get("/me/preferences");
    return d && typeof d === "object" && d.values && typeof d.values === "object" ? d : null;
  } catch {
    return null;
  }
}

/** Sends changed keys (a null value removes one). Never throws. */
export async function pushPrefs(values: Record<string, unknown>, send: (path: string, body: unknown) => Promise<unknown> = (p, b) => api.patch(p, b)): Promise<boolean> {
  try {
    await send("/me/preferences", { values });
    return true;
  } catch {
    return false;
  }
}

/** Collects pushes for a moment so a drag or a burst of clicks is one request. */
export function debouncedPusher(ms = 400, push: typeof pushPrefs = pushPrefs) {
  let pending: Record<string, unknown> = {};
  let timer: ReturnType<typeof setTimeout> | undefined;
  const flush = () => {
    timer = undefined;
    const values = pending;
    pending = {};
    if (Object.keys(values).length) void push(values);
  };
  return {
    add(key: string, value: unknown) {
      pending[key] = value;
      clearTimeout(timer);
      timer = setTimeout(flush, ms);
    },
    flush: () => {
      clearTimeout(timer);
      flush();
    },
  };
}

// ---- the layout the person chose: explorer sections, explorer width, panel height ----

export interface UiPrefs extends Stamped {
  sections?: Record<string, boolean>;
  explorerW?: number;
  panelH?: number;
}
const UI_KEY = "7ar.ui.server.v1";
export const SECTION_KEY = "7ar.ui.section.";
const SIZE_KEYS = { explorerW: "7ar.ui.explorerW", panelH: "7ar.ui.panelH" } as const;
const pusher = debouncedPusher();

function readUi(): UiPrefs {
  try {
    const v = JSON.parse(localStorage.getItem(UI_KEY) ?? "{}") as unknown;
    return v && typeof v === "object" ? (v as UiPrefs) : {};
  } catch {
    return {};
  }
}
function writeUi(v: UiPrefs) {
  try {
    localStorage.setItem(UI_KEY, JSON.stringify(v));
  } catch {
    /* a convenience */
  }
}

/** Notes a layout choice (after it was saved in its own localStorage key) and sends it to the server soon. A value that did not change is ignored. */
export function rememberUi(patch: { section?: [string, boolean]; explorerW?: number; panelH?: number }) {
  const cur = readUi();
  const next: UiPrefs = { ...cur, sections: { ...cur.sections } };
  let changed = false;
  if (patch.section && next.sections![patch.section[0]] !== patch.section[1]) {
    next.sections![patch.section[0]] = patch.section[1];
    changed = true;
  }
  for (const k of ["explorerW", "panelH"] as const) {
    const v = patch[k];
    if (v !== undefined && Math.round(v) !== next[k]) {
      next[k] = Math.round(v);
      changed = true;
    }
  }
  if (!changed) return;
  next.at = Date.now();
  writeUi(next);
  pusher.add("ui", next);
}

/** Brings the layout choices of the server into this browser when they are newer (they show from the next page load). */
export function applyUi(server: UiPrefs | undefined): void {
  const local = readUi();
  const m = mergeKey<UiPrefs>(local.at ? local : undefined, server);
  if (m.push && m.value) void pushPrefs({ ui: m.value });
  if (!m.adopted || !m.value) return;
  writeUi(m.value);
  try {
    for (const [title, open] of Object.entries(m.value.sections ?? {})) localStorage.setItem(SECTION_KEY + title, open ? "1" : "0");
    for (const k of ["explorerW", "panelH"] as const) if (typeof m.value[k] === "number") localStorage.setItem(SIZE_KEYS[k], String(m.value[k]));
  } catch {
    /* a convenience */
  }
}

let synced = "";
/** Once per page load and person: reads the document and settles the layout choices. The onboarding state is settled by its own module. */
export async function syncPrefs(login: string, onDoc: (d: PrefsDoc) => void): Promise<void> {
  if (!login || synced === login) return;
  synced = login;
  const d = await fetchPrefs();
  if (!d) {
    synced = ""; // try again at the next opportunity
    return;
  }
  applyUi(d.values.ui as UiPrefs | undefined);
  onDoc(d);
}
export const resetSyncForTests = () => {
  synced = "";
};
