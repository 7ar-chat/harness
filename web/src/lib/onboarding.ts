// What a newcomer has seen and done, kept per person in this browser (localStorage, every access guarded). Nothing here is needed to use
// the product; losing it only shows the welcome again.
import { useCallback, useSyncExternalStore } from "react";
import { useMe } from "@/api/queries";

export interface OnboardingState {
  /** The welcome flow was finished or skipped. */
  welcomed?: boolean;
  /** The guided tour was finished or skipped. */
  toured?: boolean;
  /** The getting-started checklist was dismissed. */
  dismissed?: boolean;
  /** The explorer card is folded. */
  folded?: boolean;
  /** Steps that data cannot show (like having approved an action) and that the person ticked or did. */
  marked?: Record<string, boolean>;
}

const key = (login: string) => `7ar.onboarding.v1.${login}`;
const listeners = new Set<() => void>();
const notify = () => listeners.forEach((l) => l());
let cache: { raw: string | null; value: OnboardingState; key: string } | null = null;

function read(login: string): OnboardingState {
  let raw: string | null = null;
  try {
    raw = localStorage.getItem(key(login));
  } catch {
    /* blocked storage: behave as new */
  }
  if (cache && cache.key === key(login) && cache.raw === raw) return cache.value;
  let value: OnboardingState = {};
  try {
    const v = raw ? (JSON.parse(raw) as unknown) : {};
    if (v && typeof v === "object") value = v as OnboardingState;
  } catch {
    /* corrupt value */
  }
  cache = { raw, value, key: key(login) };
  return value;
}

export function patchOnboarding(login: string, patch: Partial<OnboardingState>) {
  const next = { ...read(login), ...patch };
  try {
    localStorage.setItem(key(login), JSON.stringify(next));
  } catch {
    /* storage is a convenience */
  }
  cache = null;
  notify();
}

/** Remembers a step as done from anywhere (outside React too), for the person currently signed in. */
let currentLogin = "";
export function markStep(id: string) {
  if (!currentLogin) return;
  const s = read(currentLogin);
  if (s.marked?.[id]) return;
  patchOnboarding(currentLogin, { marked: { ...s.marked, [id]: true } });
}

if (typeof window !== "undefined") window.addEventListener("7ar:step", (e) => markStep(String((e as CustomEvent).detail)));

const subscribe = (l: () => void) => {
  listeners.add(l);
  window.addEventListener("storage", l);
  return () => {
    listeners.delete(l);
    window.removeEventListener("storage", l);
  };
};

const NONE: OnboardingState = {};

export function useOnboarding() {
  const login = useMe().data?.user.login ?? "";
  currentLogin = login || currentLogin;
  const state = useSyncExternalStore(subscribe, () => read(login), () => NONE);
  const patch = useCallback((p: Partial<OnboardingState>) => patchOnboarding(login, p), [login]);
  return { login, state, patch };
}

/** Whether the welcome, tour and checklist show up by themselves. They are off in unit tests (so no other test meets a dialog) unless a test asks. */
export function onboardingAuto(): boolean {
  if (import.meta.env.MODE !== "test") return true;
  try {
    return localStorage.getItem("7ar.onboarding.force") === "1";
  } catch {
    return false;
  }
}

// Requests from menus and the palette to the components that own the flows.
export const ONBOARDING_EVENTS = { welcome: "7ar:welcome", tour: "7ar:tour", shortcuts: "7ar:shortcuts" } as const;
export const requestOnboarding = (what: keyof typeof ONBOARDING_EVENTS) => window.dispatchEvent(new CustomEvent(ONBOARDING_EVENTS[what]));
