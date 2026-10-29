// Sound cues synthesized with Web Audio (no audio files): a chime when a run finishes,
// a more insistent tone when a session needs a person.
import { useSyncExternalStore } from "react";
import type { FeedEvent } from "@/api/types";

export type Sound = "success" | "help" | "failed";

export interface SoundContext {
  /** Login of the current user. */
  me?: string;
  /** Thread being looked at in a focused tab, if any. */
  watching?: string;
}

/** Which sound, if any, a feed event deserves. Pure. */
export function soundFor(ev: Pick<FeedEvent, "type" | "status" | "actor" | "threadId">, ctx: SoundContext): Sound | null {
  if (ev.type !== "run.finished") return null;
  let sound: Sound | null = null;
  if (ev.status === "completed") sound = "success";
  else if (ev.status === "waiting_approval" || ev.status === "waiting_input") sound = "help";
  else if (ev.status === "failed") sound = "failed";
  if (!sound) return null;
  if (ev.threadId && ev.threadId === ctx.watching) return null;
  const mine = ev.actor !== undefined && ev.actor === ctx.me;
  if (mine && sound === "success") return null;
  return sound;
}

const priority: Record<Sound, number> = { success: 0, failed: 1, help: 2 };
/** In a burst, the sound that needs the person most wins. */
export const strongest = (a: Sound | null, b: Sound): Sound => (a === null || priority[b] > priority[a] ? b : a);

/**
 * Collects sounds for `windowMs`, plays the strongest, then stays quiet for `gapMs`
 * (so a burst plays at most one sound per ~1.5 s).
 */
export function createScheduler(play: (s: Sound) => void, o: { windowMs?: number; gapMs?: number; now?: () => number; setTimer?: (f: () => void, ms: number) => unknown } = {}) {
  const { windowMs = 250, gapMs = 1500, now = Date.now, setTimer = (f, ms) => setTimeout(f, ms) } = o;
  let pending: Sound | null = null;
  let quietUntil = 0;
  let armed = false;
  return (s: Sound) => {
    if (now() < quietUntil && !armed) return;
    pending = strongest(pending, s);
    if (armed) return;
    armed = true;
    setTimer(() => {
      armed = false;
      const p = pending;
      pending = null;
      quietUntil = now() + gapMs;
      if (p) play(p);
    }, windowMs);
  };
}

// ---- synthesis ----
let ctx: AudioContext | null = null;
function audio(): AudioContext | null {
  try {
    if (!ctx) {
      const C = window.AudioContext ?? (window as unknown as { webkitAudioContext?: typeof AudioContext }).webkitAudioContext;
      if (!C) return null;
      ctx = new C();
    }
    return ctx;
  } catch {
    return null;
  }
}

/** Autoplay policy: the context may only start after a user gesture. */
export function unlockAudioOnGesture() {
  const unlock = (e: Event) => {
    // Modifier keys alone are not a user activation: creating the context then only logs a warning.
    if (e instanceof KeyboardEvent && ["Control", "Meta", "Alt", "Shift", "Escape"].includes(e.key)) return;
    const c = audio();
    void c?.resume().catch(() => undefined);
    window.removeEventListener("pointerdown", unlock);
    window.removeEventListener("keydown", unlock);
  };
  window.addEventListener("pointerdown", unlock);
  window.addEventListener("keydown", unlock);
}

interface Note {
  freq: number;
  at: number;
  dur: number;
  type: OscillatorType;
}
const SCORES: Record<Sound, Note[]> = {
  success: [
    { freq: 659.25, at: 0, dur: 0.16, type: "sine" },
    { freq: 987.77, at: 0.12, dur: 0.3, type: "sine" },
  ],
  help: [
    { freq: 880, at: 0, dur: 0.11, type: "triangle" },
    { freq: 880, at: 0.19, dur: 0.11, type: "triangle" },
    { freq: 1175, at: 0.38, dur: 0.18, type: "triangle" },
  ],
  failed: [
    { freq: 392, at: 0, dur: 0.18, type: "triangle" },
    { freq: 261.63, at: 0.16, dur: 0.32, type: "triangle" },
  ],
};

export function playSound(s: Sound, volume = 0.5) {
  const c = audio();
  if (!c) return;
  try {
    void c.resume().catch(() => undefined);
    const t0 = c.currentTime + 0.02;
    const master = c.createGain();
    master.gain.value = Math.max(0, Math.min(1, volume)) * 0.35;
    master.connect(c.destination);
    for (const n of SCORES[s]) {
      const osc = c.createOscillator();
      const g = c.createGain();
      osc.type = n.type;
      osc.frequency.value = n.freq;
      g.gain.setValueAtTime(0.0001, t0 + n.at);
      g.gain.exponentialRampToValueAtTime(1, t0 + n.at + 0.015);
      g.gain.exponentialRampToValueAtTime(0.0001, t0 + n.at + n.dur);
      osc.connect(g).connect(master);
      osc.start(t0 + n.at);
      osc.stop(t0 + n.at + n.dur + 0.05);
    }
  } catch {
    /* Web Audio unavailable or blocked: stay silent */
  }
}

// ---- preferences (localStorage, per viewer) ----
export interface SoundPrefs {
  enabled: boolean;
  volume: number;
}
const KEY = "harness.sound";
let prefs: SoundPrefs | null = null;
const listeners = new Set<() => void>();
export function getSoundPrefs(): SoundPrefs {
  if (prefs) return prefs;
  prefs = { enabled: true, volume: 0.6 };
  try {
    const raw = localStorage.getItem(KEY);
    if (raw) prefs = { ...prefs, ...(JSON.parse(raw) as Partial<SoundPrefs>) };
  } catch {
    /* storage unavailable */
  }
  return prefs;
}
export function setSoundPrefs(p: Partial<SoundPrefs>) {
  prefs = { ...getSoundPrefs(), ...p };
  try {
    localStorage.setItem(KEY, JSON.stringify(prefs));
  } catch {
    /* storage unavailable */
  }
  listeners.forEach((l) => l());
}
export const useSoundPrefs = () =>
  useSyncExternalStore(
    (l) => (listeners.add(l), () => listeners.delete(l)),
    getSoundPrefs,
  );

/** Plays through the scheduler, honoring the preferences. */
export const cue = createScheduler((s) => {
  const p = getSoundPrefs();
  if (p.enabled) playSound(s, p.volume);
});
