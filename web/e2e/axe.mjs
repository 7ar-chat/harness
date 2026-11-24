// Accessibility checks for the e2e: axe-core is injected into the page from node_modules (no network).
// A scan FAILS on every serious or critical violation and on the rules in ALWAYS_FAIL whatever their impact (landmarks and
// heading structure). Nothing is allow-listed silently: an exception is an entry in ALLOW with the reason on the same line.
import fs from "node:fs";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const AXE_SRC = fs.readFileSync(require.resolve("axe-core/axe.min.js"), "utf8");

export const FAIL_IMPACTS = new Set(["serious", "critical"]);
export const ALWAYS_FAIL = new Set(["region", "landmark-one-main", "landmark-unique", "landmark-no-duplicate-main", "heading-order", "empty-heading", "tabindex", "focus-order-semantics", "target-size"]);
// { rule, within (a CSS selector the node must be inside, optional), reason }
export const ALLOW = [
  // Text inside a hidden-by-opacity transition frame is measured mid-fade; the settled state is checked by the next scan of the same page.
  // (no entries needed so far: keep this list empty unless a reason is written here)
];

const allowed = (rule, nodeTarget) => ALLOW.some((a) => a.rule === rule && (!a.within || nodeTarget.some((t) => String(t).includes(a.within))));

/** Runs axe on the page (or on `include`, a CSS selector) and returns the failing violations as {id, impact, help, nodes[]}. */
export async function axeScan(pg, { include } = {}) {
  if (!(await pg.evaluate(() => typeof window.axe !== "undefined"))) await pg.addScriptTag({ content: AXE_SRC });
  const raw = await pg.evaluate(async (inc) => {
    const r = await window.axe.run(inc ? { include: [[inc]] } : document, { resultTypes: ["violations"] });
    return r.violations.map((v) => ({ id: v.id, impact: v.impact, help: v.help, nodes: v.nodes.map((n) => ({ target: n.target.flat(), html: n.html.slice(0, 160), ancestry: n.ancestry?.flat().join(" > ") })) }));
  }, include ?? null);
  const out = [];
  for (const v of raw) {
    if (!FAIL_IMPACTS.has(v.impact) && !ALWAYS_FAIL.has(v.id)) continue;
    const nodes = v.nodes.filter((n) => !allowed(v.id, n.target));
    if (nodes.length) out.push({ ...v, nodes });
  }
  return out;
}

export const describe = (label, vs) => vs.map((v) => `${label}: ${v.id} (${v.impact}) x${v.nodes.length} ${v.nodes.slice(0, 2).map((n) => n.target.join(" ") + " :: " + n.html + " @ " + (n.ancestry ?? "")).join(" | ")}`);
