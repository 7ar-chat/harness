// Screenshots of the main screens against the mock server; fails on console errors (CSP violations included).
import { chromium } from "playwright";
const base = process.argv[2] ?? "http://127.0.0.1:8099";
const out = process.argv[3] ?? "/tmp/harness-shots";
import fs from "node:fs";
fs.mkdirSync(out, { recursive: true });
const browser = await chromium.launch();
const errors = [];
async function shot(name, hash, { w = 1440, h = 900, dark = false, act } = {}) {
  const ctx = await browser.newContext({ viewport: { width: w, height: h }, colorScheme: dark ? "dark" : "light" });
  const page = await ctx.newPage();
  page.on("console", (m) => (m.type() === "error" || m.type() === "warning") && errors.push(`${name}: ${m.text()}`));
  page.on("pageerror", (e) => errors.push(`${name}: ${e.message}`));
  await page.goto(`${base}/#${hash}`);
  await page.waitForTimeout(1200);
  if (act) await act(page);
  await page.screenshot({ path: `${out}/${name}.png` });
  await ctx.close();
}
await shot("dashboard", "/");
await shot("dashboard-dark", "/", { dark: true });
await shot("session", "/t/t1");
await shot("session-tools-open", "/t/t1", { act: async (p) => { for (const t of await p.locator("button:has-text('edit_file')").all()) await t.click(); await p.waitForTimeout(800); } });
await shot("approval", "/t/t2");
await shot("supervisor", "/supervisor");
await shot("project", "/p/p1");
await shot("project-members", "/p/p1?tab=members");
await shot("project-settings", "/p/p1?tab=settings");
await shot("memory", "/memory");
await shot("session-mobile", "/t/t1", { w: 390, h: 800 });
await shot("dashboard-mobile", "/", { w: 390, h: 800 });
await shot("notfound", "/t/nope");
await shot("palette", "/", { act: async (p) => { await p.keyboard.press("Control+k"); await p.waitForTimeout(400); } });
await browser.close();
console.log(errors.length ? "ERRORS:\n" + [...new Set(errors)].join("\n") : "no console errors");
