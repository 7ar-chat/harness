// Production-shaped end-to-end suite: `npm run e2e:stack` from web/ (opt-in; never part of `npm run e2e`).
// Real OIDC (Dex) sign-in, several people, two harness-web replicas behind a round-robin proxy, a separate
// harness-worker, Postgres + Redis + S3 (RustFS) in Docker. See docs/e2e-stack.md.
//   node e2e/stack/run.mjs [--only s1,s3] [--keep-logs]
// Exit codes: 0 = PASS or SKIP (prerequisite missing, said on stdout), 1 = FAIL.
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { pathToFileURL, fileURLToPath } from "node:url";
import { makeEnv } from "./lib.mjs";
import { dockerAvailable, startStack, tailAll } from "./stack.mjs";

const here = path.dirname(fileURLToPath(import.meta.url));
const skip = (why) => {
  console.log(`SKIP: ${why}`);
  process.exit(0);
};
const args = process.argv.slice(2);
const only = args.includes("--only") ? args[args.indexOf("--only") + 1].split(",") : null;

if (spawnSync("go", ["version"], { stdio: "ignore" }).status !== 0) skip("`go` is not available on PATH");
if (!dockerAvailable()) skip("Docker is not available (the stack needs Postgres, Redis, Dex and S3 containers): start Docker and retry");
let chromium;
try {
  ({ chromium } = await import("playwright"));
} catch (e) {
  skip(`the playwright package is not installed (${e.code ?? e.message}): run npm ci in web/`);
}
let browser;
try {
  browser = await chromium.launch({ handleSIGINT: false, handleSIGTERM: false, handleSIGHUP: false });
} catch (e) {
  skip(`playwright cannot launch Chromium (${String(e.message).split("\n")[0]}): npx playwright install chromium`);
}

const ALL = ["s1", "s2", "s3", "s4", "s5", "s6", "s7"];
const failDir = path.join(os.tmpdir(), "harness-stack-failures");
const t0 = Date.now();
let S = null;
let cleaning = null;
const cleanup = () =>
  (cleaning ??= (async () => {
    await browser?.close().catch(() => {});
    await S?.stop();
  })());
for (const sig of ["SIGINT", "SIGTERM", "SIGHUP"]) process.on(sig, () => void cleanup().finally(() => process.exit(130)));

const results = [];
const problems = [];
let failed = false;
try {
  S = await startStack({ log: (m) => console.log(m), keepLogs: args.includes("--keep-logs") });
  const env = makeEnv(S, browser, problems);
  for (const id of ALL) {
    if (only && !only.includes(id)) continue;
    const mod = await import(pathToFileURL(path.join(here, "scenarios", `${id}.mjs`)).href);
    const started = Date.now();
    console.log(`\n== ${id}: ${mod.title}`);
    for (const [name, fn] of mod.steps) {
      const s = Date.now();
      try {
        await fn(env);
        results.push({ id, name, ok: true });
        console.log(`  PASS ${name} (${((Date.now() - s) / 1000).toFixed(1)}s)`);
      } catch (e) {
        failed = true;
        results.push({ id, name, ok: false, err: e.message });
        console.log(`  FAIL ${name}\n       ${String(e.stack ?? e.message).split("\n").slice(0, 6).join("\n       ")}`);
        fs.mkdirSync(failDir, { recursive: true });
        for (const [who, p] of env.people) {
          if (p.page && !p.page.isClosed()) await p.page.screenshot({ path: path.join(failDir, `${id}-${name.replace(/\W+/g, "_").slice(0, 40)}-${who}.png`) }).catch(() => {});
        }
        break; // the steps of a scenario depend on each other: go to the next scenario
      }
    }
    if (mod.after) await mod.after(env).catch((e) => console.log(`  (cleanup of ${id}: ${e.message})`));
    console.log(`== ${id} done in ${((Date.now() - started) / 1000).toFixed(0)}s`);
  }
  if (failed) {
    await tailAll(S, console.log, 40);
    console.log(`screenshots: ${failDir}`);
  }
} catch (e) {
  failed = true;
  console.log(`FAIL (setup): ${e.stack ?? e}`);
} finally {
  await cleanup();
}
if (problems.length) console.log(`\npage errors seen:\n  ${[...new Set(problems)].join("\n  ")}`);
const pass = results.filter((r) => r.ok).length;
console.log(`\n${failed ? "FAIL" : "PASS"}: ${pass}/${results.length} steps in ${((Date.now() - t0) / 1000).toFixed(0)}s`);
process.exit(failed ? 1 : 0);
