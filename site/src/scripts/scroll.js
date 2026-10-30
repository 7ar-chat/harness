// Scroll behaviour of the home page. Everything here is progressive: without this script (or with
// prefers-reduced-motion) all content is visible and nothing is pinned.
const reduce = matchMedia("(prefers-reduced-motion: reduce)").matches;
document.documentElement.dataset.ready = "1";

// Reveal blocks as they enter the viewport.
const reveals = document.querySelectorAll("[data-reveal]");
if (reduce || !("IntersectionObserver" in window)) {
  reveals.forEach((el) => el.classList.add("in"));
} else {
  const io = new IntersectionObserver((entries) => {
    for (const e of entries) if (e.isIntersecting) { e.target.classList.add("in"); io.unobserve(e.target); }
  }, { rootMargin: "0px 0px -12% 0px", threshold: 0.08 });
  reveals.forEach((el) => io.observe(el));
}

// Replay: lines of a run appear one after another once the window is on screen.
document.querySelectorAll(".replay").forEach((root) => {
  const items = [...root.querySelectorAll(".line, .result")];
  const show = (el) => el.classList.add("on");
  if (reduce || !("IntersectionObserver" in window)) { items.forEach(show); return; }
  let started = false;
  const start = () => {
    if (started) return; started = true;
    // Each item appears after the previous one (data-gap ms); data-with="N" shows it together with item N.
    const at = [];
    let t = 500;
    items.forEach((el, i) => {
      at[i] = el.dataset.with !== undefined ? at[Number(el.dataset.with)] : t;
      if (el.dataset.with === undefined) t += Number(el.dataset.gap || 650);
    });
    items.forEach((el, i) => setTimeout(() => show(el), at[i]));
  };
  new IntersectionObserver((es, o) => { if (es.some((e) => e.isIntersecting)) { start(); o.disconnect(); } }, { threshold: 0.25 }).observe(root);
});

// Story: the step nearest the middle of the screen decides which frame is pinned beside the text.
const steps = [...document.querySelectorAll(".story-step")];
const frames = [...document.querySelectorAll(".story .frame")];
if (steps.length && frames.length) {
  const set = (i) => {
    steps.forEach((s, j) => s.classList.toggle("on", j === i));
    frames.forEach((f, j) => f.classList.toggle("on", j === i));
  };
  set(0);
  const pick = () => {
    const mid = innerHeight * 0.45;
    let best = 0, bestD = Infinity;
    steps.forEach((s, i) => {
      const r = s.getBoundingClientRect();
      const d = Math.abs(r.top + Math.min(r.height, innerHeight * 0.5) / 2 - mid);
      if (d < bestD) { bestD = d; best = i; }
    });
    set(best);
  };
  let tick = false;
  addEventListener("scroll", () => { if (!tick) { tick = true; requestAnimationFrame(() => { tick = false; pick(); }); } }, { passive: true });
  addEventListener("resize", pick);
  pick();
}
