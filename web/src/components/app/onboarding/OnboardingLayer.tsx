// Mounted once in the shell: opens the welcome the first time, and owns the welcome, the tour and the shortcuts sheet for everyone who asks
// (user menu, palette, help center, the "?" key).
import { useEffect, useMemo, useState } from "react";
import { useOverview } from "@/api/queries";
import { useDialogs } from "@/components/app/dialogs";
import { useShell } from "@/components/app/shell/context";
import { ONBOARDING_EVENTS, onboardingAuto, useOnboarding } from "@/lib/onboarding";
import { ShortcutsSheet } from "./Shortcuts";
import { Tour, SHELL_TOUR, type TourStep } from "./Tour";
import { Welcome } from "./Welcome";
import { useTrySample } from "./sample";

const typing = (t: EventTarget | null) => {
  const el = t as HTMLElement | null;
  return !!el && (el.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(el.tagName) || el.closest?.('[role="textbox"]') !== null);
};

export function OnboardingLayer() {
  const { login, state, patch, ready } = useOnboarding();
  const overviewLoaded = !!useOverview().data;
  const { newProject, newSession } = useDialogs();
  const { setPanel, setExplorer, narrow } = useShell();
  const sample = useTrySample();
  const [welcome, setWelcome] = useState(false);
  const [tour, setTour] = useState(false);
  const [keys, setKeys] = useState(false);
  const [auto, setAuto] = useState(false);

  // first visit: the welcome opens once the person and their data are known
  useEffect(() => {
    if (auto || !login || !ready || !overviewLoaded || state.welcomed || !onboardingAuto()) return;
    setAuto(true);
    setWelcome(true);
  }, [auto, login, ready, overviewLoaded, state.welcomed]);

  useEffect(() => {
    const w = () => setWelcome(true);
    const t = () => setTour(true);
    const k = () => setKeys(true);
    window.addEventListener(ONBOARDING_EVENTS.welcome, w);
    window.addEventListener(ONBOARDING_EVENTS.tour, t);
    window.addEventListener(ONBOARDING_EVENTS.shortcuts, k);
    const h = (e: KeyboardEvent) => {
      if (e.key === "?" && !e.metaKey && !e.ctrlKey && !e.altKey && !typing(e.target) && !document.querySelector('[role="dialog"]')) {
        e.preventDefault();
        setKeys(true);
      }
    };
    window.addEventListener("keydown", h);
    return () => {
      window.removeEventListener(ONBOARDING_EVENTS.welcome, w);
      window.removeEventListener(ONBOARDING_EVENTS.tour, t);
      window.removeEventListener(ONBOARDING_EVENTS.shortcuts, k);
      window.removeEventListener("keydown", h);
    };
  }, []);

  const steps = useMemo<TourStep[]>(
    () =>
      SHELL_TOUR.map((s) => ({
        ...s,
        before: () => {
          if (narrow) return;
          if (s.id === "explorer") setExplorer(true);
          if (s.id === "needs") setPanel(true);
        },
      })),
    [narrow, setExplorer, setPanel],
  );

  return (
    <>
      <Welcome
        open={welcome}
        onClose={() => {
          setWelcome(false);
          patch({ welcomed: true });
        }}
        actions={{ newProject, startSession: () => newSession(), trySample: () => void sample.start() }}
      />
      <Tour open={tour} steps={steps} onClose={() => (setTour(false), patch({ toured: true }))} />
      <ShortcutsSheet open={keys} onOpenChange={setKeys} />
    </>
  );
}
