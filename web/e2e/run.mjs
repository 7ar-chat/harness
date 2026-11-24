// Browser end-to-end test: a REAL harness-web process (built from ../app) + a scripted fake LLM (fakellm.mjs) +
// headless Chromium (playwright). No external network. Run it with `npm run e2e` from web/.
//
// Exit codes: 0 = PASS or SKIP (prerequisite missing, said on stdout), 1 = FAIL.
// Fails on any console error/warning, pageerror or CSP violation (nothing is allow-listed except what is
// documented at BENIGN below). Temp dirs and child processes are always cleaned up, also on failure and SIGINT.
import { spawn, spawnSync } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { startFakeLlm } from "./fakellm.mjs";

const here = path.dirname(fileURLToPath(import.meta.url));
const appDir = path.resolve(here, "../../app");
const T = 20_000; // default wait for a UI condition

const skip = (why) => {
  console.log(`SKIP: ${why}`);
  process.exit(0);
};

// ---- prerequisites --------------------------------------------------------------------------------------------
if (spawnSync("go", ["version"], { stdio: "ignore" }).status !== 0) skip("`go` is not available on PATH");
let chromium;
try {
  ({ chromium } = await import("playwright"));
} catch (e) {
  skip(`the playwright package is not installed (${e.code ?? e.message})`);
}
let browser;
try {
  browser = await chromium.launch({ handleSIGINT: false, handleSIGTERM: false, handleSIGHUP: false }); // our own handlers clean up (playwright's would exit first)
} catch (e) {
  skip(`playwright cannot launch Chromium (${String(e.message).split("\n")[0]})`);
}

// ---- cleanup --------------------------------------------------------------------------------------------------
const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "harness-e2e-"));
let server = null;
const extraServers = []; // more harness-web processes a scenario started (m5)
let fake = null;
let hookSrv = null; // the webhook receiver of the notification scenarios (l*)
let cleanupDone = null;
const cleanup = () => (cleanupDone ??= doCleanup()); // one run, awaited by every caller (SIGINT and the normal exit)
async function doCleanup() {
  await browser?.close().catch(() => {});
  if (server && server.exitCode === null) {
    server.kill("SIGTERM");
    const gone = await Promise.race([new Promise((r) => server.once("exit", () => r(true))), new Promise((r) => setTimeout(() => r(false), 6000))]);
    if (!gone) server.kill("SIGKILL");
  }
  for (const x of extraServers) if (x.exitCode === null) x.kill("SIGKILL");
  await fake?.close().catch(() => {});
  hookSrv?.close();
  hookSrv?.closeAllConnections?.();
  fs.rmSync(tmp, { recursive: true, force: true });
}
for (const sig of ["SIGINT", "SIGTERM", "SIGHUP"]) {
  process.on(sig, () => void cleanup().finally(() => process.exit(130)));
}

// ---- build + start the server ---------------------------------------------------------------------------------
const results = [];
const problems = []; // console errors/warnings, page errors, CSP violations
let failed = false;
const failDir = path.join(os.tmpdir(), "harness-e2e-failures");

async function main() {
  const bin = path.join(tmp, "harness-web");
  console.log("building harness-web …");
  const build = spawnSync("go", ["build", "-o", bin, "./cmd/harness-web"], { cwd: appDir, encoding: "utf8" });
  if (build.status !== 0) throw new Error(`go build failed:\n${build.stderr || build.stdout}`);

  fake = await startFakeLlm();
  // A tiny webhook receiver on loopback: it records what it is sent and answers with `hook.code`.
  const hook = { calls: [], code: 200 };
  hookSrv = http.createServer((req, res) => {
    const chunks = [];
    req.on("data", (c) => chunks.push(c));
    req.on("end", () => {
      hook.calls.push({ headers: req.headers, body: Buffer.concat(chunks).toString("utf8") });
      res.writeHead(hook.code).end();
    });
  });
  await new Promise((r) => hookSrv.listen(0, "127.0.0.1", r));
  hook.url = `http://127.0.0.1:${hookSrv.address().port}/e2e-hook`;
  const dirs = { home: "home", ws: "workspace", state: "state" };
  for (const d of Object.values(dirs)) fs.mkdirSync(path.join(tmp, d));
  const ws = path.join(tmp, dirs.ws);
  // In team mode every session has its own sandbox directory <state>/<sandbox id>/workspace (LOCAL_WORKSPACE is
  // not used), so side effects of the agent's commands are looked up by file name under the state dir.
  const stateDir = path.join(tmp, dirs.state);
  const findFile = (name) => {
    const walk = (d) => {
      for (const e of fs.readdirSync(d, { withFileTypes: true })) {
        const p = path.join(d, e.name);
        if (e.isDirectory()) {
          const r = walk(p);
          if (r) return r;
        } else if (e.name === name && p.includes(`${path.sep}workspace${path.sep}`)) return p;
      }
      return null;
    };
    return walk(stateDir);
  };

  const pricesFile = path.join(tmp, "prices.json");
  fs.writeFileSync(pricesFile, JSON.stringify({ "gpt-test": { mode: "chat", litellm_provider: "openai", max_input_tokens: 128000, max_output_tokens: 8000, input_cost_per_token: 1e-3, output_cost_per_token: 1e-3 } }));
  const env = {
    PATH: process.env.PATH,
    HOME: path.join(tmp, dirs.home),
    LOCAL_WORKSPACE: ws,
    LOCAL_STATE_DIR: path.join(tmp, dirs.state),
    LLM_PROVIDER: "openai",
    LLM_BASE_URL: fake.url,
    LLM_API_KEY: "test",
    LLM_MODELS: "gpt-test",
    LLM_DEFAULT_MODEL: "gpt-test",
    LLM_STEP_RETRIES: "0",
    // No download of the public price table: a one-model table prices the fake model (1e-3 USD per token), so the
    // usage totals and the spend cap (h2) are deterministic: one scripted call costs $0.015.
    HARNESS_MODEL_PRICES: pricesFile,
    HARNESS_WEB_ADDR: "127.0.0.1:0",
    // The UI needs the team API (/api/me, /api/overview: without -team they answer 404 and the dashboard shows
    // "Not found", see the report), so the server runs in team mode without OIDC: everyone is the user "local".
    // -team refuses the local sandbox unless the risk is accepted; this is a loopback test server.
    HARNESS_ALLOW_LOCAL_SANDBOX: "1",
    HARNESS_MEMORY_EXTRACT: "false", // team mode would otherwise add memory-extraction LLM calls after each run
    MEMORY_IN_CODING: "false",
    // The agent factory without a code host: a task is run as an agent session and its answer is the result. It turns
    // on the Tasks tab and the inbox's task list (scenarios g and h).
    FACTORY: "1",
    FACTORY_NO_REVIEW: "1",
    // A failed attempt is retried after 30s by default; one second keeps the scenarios that refuse a task (m6) quick.
    FACTORY_RETRY_BACKOFF_SECONDS: "1",
    // The admin page and the git hosts need a secret key (to seal tokens and SSH keys) and an administrator.
    HARNESS_SECRET_KEY: "e2e-secret-key-0123456789abcdef0123456789",
    // E2E_POSTGRES=host:port:user:password:database runs the whole app on Postgres instead of its file stores (the stores
    // have conformance suites, this proves the real flows on them). A fresh schema per run keeps runs apart.
    ...(process.env.E2E_POSTGRES
      ? (([host, port, user, password, db]) => ({
          POSTGRES_HOST: host, POSTGRES_PORT: port, POSTGRES_USER: user, POSTGRES_PASSWORD: password, POSTGRES_DB: db,
          POSTGRES_SSLMODE: "disable", POSTGRES_SCHEMA: `e2e_${Date.now()}`,
        }))(process.env.E2E_POSTGRES.split(":"))
      : {}),
    HARNESS_ADMINS: "local",
    HARNESS_NOTIFY_ALLOW_PRIVATE: "1", // the webhook receiver above is on loopback (scenarios l*)
    // The document library and the job search are optional features whose routes answer 404 without a directory; they are on
    // so that the References and Jobs pages render their real state (h5) instead of a "not available" one.
    HARNESS_DOCUMENTS_DIR: path.join(tmp, "documents"),
    HARNESS_JOBS_DIR: path.join(tmp, "jobs"),
  };
  server = spawn(bin, ["-local", "-team"], { cwd: tmp, env, stdio: ["ignore", "pipe", "pipe"] });
  let serverLog = "";
  server.stderr.on("data", (d) => (serverLog += d));
  if (process.env.E2E_SERVER_LOG) server.stderr.on("data", (d) => fs.appendFileSync(process.env.E2E_SERVER_LOG, d)); // opt-in: keep the server log for debugging
  const base = await new Promise((resolve, reject) => {
    let out = "";
    const timer = setTimeout(() => reject(new Error(`harness-web did not report its address in 30s:\n${out}${serverLog}`)), 30_000);
    server.stdout.on("data", (d) => {
      out += d;
      const m = /harness-web: (http:\/\/[^/\s]+)\//.exec(out);
      if (m) (clearTimeout(timer), resolve(m[1]));
    });
    server.once("exit", (code) => (clearTimeout(timer), reject(new Error(`harness-web exited early (${code}):\n${out}${serverLog}`))));
  });
  console.log(`harness-web at ${base}, fake LLM at ${fake.url}`);

  const api = async (p, init) => {
    const r = await fetch(`${base}/api${p}`, init);
    if (!r.ok) throw new Error(`${init?.method ?? "GET"} /api${p} -> ${r.status}`);
    return r.json();
  };

  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const watch = (page) => {
    page.on("console", (m) => (m.type() === "error" || m.type() === "warning") && problems.push(`console.${m.type()}: ${m.text()}`));
    page.on("pageerror", (e) => problems.push(`pageerror: ${e.message}`));
  };
  ctx.on("page", watch);
  // The welcome, the tour and the checklist are for newcomers: this browser is marked as onboarded so they do not cover the other
  // scenarios. The i* scenarios use a fresh browser context (a new person) instead.
  const onboarded = () => {
    try {
      localStorage.setItem("7ar.onboarding.v1.local", JSON.stringify({ welcomed: true, toured: true, dismissed: true }));
    } catch {
      /* a sandboxed frame has no storage */
    }
  };
  await ctx.addInitScript(onboarded);
  const page = await ctx.newPage();

  // ---- assertion helpers (observable outcomes only; waits are locator/poll waits, never fixed sleeps) ----------
  // Tabs the person left stay mounted (hidden): only what is actually on screen counts.
  const visible = (loc, what, timeout = T) => loc.filter({ visible: true }).first().waitFor({ state: "visible", timeout }).catch(() => Promise.reject(new Error(`expected visible: ${what}`)));
  const gone = (loc, what, timeout = T) => loc.filter({ visible: true }).first().waitFor({ state: "hidden", timeout }).catch(() => Promise.reject(new Error(`expected gone: ${what}`)));
  const eq = (got, want, what) => {
    if (JSON.stringify(got) !== JSON.stringify(want)) throw new Error(`${what}: got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`);
  };
  const poll = async (fn, what, timeout = T) => {
    const end = Date.now() + timeout;
    for (;;) {
      const v = await fn();
      if (v) return v;
      if (Date.now() > end) throw new Error(`timed out waiting for: ${what}`);
      await new Promise((r) => setTimeout(r, 100));
    }
  };

  const composer = page.getByRole("textbox", { name: "Message" });
  const send = async (text) => {
    await composer.fill(text);
    await page.getByRole("button", { name: "Send", exact: true }).click();
  };
  // "New session" opens an untitled session at once (there is no title dialog any more); the title is set through the API.
  const newSession = async (title) => {
    await page.goto(`${base}/#/`);
    await page.getByRole("button", { name: "New session", exact: true }).click();
    await page.waitForURL(/#\/t\/[^/]+$/);
    await visible(composer, "composer");
    const id = page.url().split("/t/")[1];
    await api(`/threads/${id}`, { method: "PATCH", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ title }) });
    return id;
  };
  const thread = (id) => api(`/threads/${id}`);
  const idle = (id) => poll(async () => (await thread(id)).status === "idle" || (await thread(id)).status === "done", `thread ${id} idle`);
  const roles = (t) => t.messages.map((m) => m.role);
  const sessionState = { w: null, d: null };

  // E2E_ONLY=h1,h4 runs just those scenarios (matched on the name's prefix before the dot) while developing one.
  let shotPage = page; // the page a failure screenshot is taken of
  const only = process.env.E2E_ONLY?.split(",");
  async function step(name, fn) {
    if (only && !only.includes(name.split(".")[0])) return;
    const t0 = Date.now();
    const before = problems.length;
    try {
      await fn();
      if (problems.length > before) throw new Error(`browser problems:\n  ${problems.slice(before).join("\n  ")}`);
      results.push({ name, ok: true, ms: Date.now() - t0 });
    } catch (e) {
      failed = true;
      results.push({ name, ok: false, ms: Date.now() - t0, err: e.stack ?? String(e) });
      fs.mkdirSync(failDir, { recursive: true });
      const shot = path.join(failDir, `${name.replace(/\W+/g, "-")}.png`);
      await shotPage.screenshot({ path: shot }).then(() => (results.at(-1).shot = shot)).catch(() => {});
      // The run goes on: a scenario that does not depend on the failed one still says whether it passes.
    }
  }

  // a. new conversation, send, answer streams
  await step("a. new conversation: the answer streams in", async () => {
    sessionState.w = await newSession("E2E write");
    await send("please write a file");
    await visible(page.getByText("please write a file", { exact: true }), "the user message");
    // The final reply is held before its last piece: the partial text is on screen while the run is still going.
    await fake.held();
    await visible(page.getByText("I wrote report.md and nothing else", { exact: true }), "the partial answer");
    await visible(page.getByText(/^Running/), "the Running status while streaming");
    eq(await page.getByText("I wrote report.md and nothing else.").count(), 0, "the final text must not be there yet");
    fake.release();
    await visible(page.getByText("I wrote report.md and nothing else."), "the final answer");
    await visible(page.getByText("Idle", { exact: true }), "Idle status after the run");
    const t = await idle(sessionState.w).then(() => thread(sessionState.w));
    eq(roles(t), ["user", "assistant", "tool", "assistant"], "server-side message roles");
    eq(t.messages.at(-1).content, "I wrote report.md and nothing else.", "stored final answer");
  });

  // b. tool card
  await step("b. the write_file tool call is a card that expands to its arguments and result", async () => {
    // the tool steps of a reply are folded under "Worked N step(s)"
    await page.getByRole("button", { name: /Worked/ }).first().click();
    const card = page.getByRole("button", { name: /write_file/ });
    await visible(card, "the write_file card");
    eq(await card.first().getAttribute("aria-expanded"), "false", "card starts collapsed");
    await card.first().click();
    eq(await card.first().getAttribute("aria-expanded"), "true", "card expanded");
    // arguments (JSON) and result (JSON from the tool) are both shown as <pre>
    await visible(page.locator("pre").filter({ hasText: '"content":"# Report v1\\n"' }), "the arguments");
    await visible(page.locator("pre").filter({ hasText: "bytesWritten" }), "the tool result");
    const t = await thread(sessionState.w);
    const call = t.messages.find((m) => m.toolCalls?.length)?.toolCalls[0];
    eq(call?.function?.name ?? call?.name, "write_file", "stored tool call name");
  });

  // c. artifact in the files panel
  await step("c. report.md appears in the files panel and previews", async () => {
    eq(fs.readFileSync(findFile("report.md") ?? "missing-report.md", "utf8"), "# Report v1\n", "file on disk");
    const row = page.getByRole("treeitem").filter({ hasText: "report.md" });
    await visible(row, "report.md in the files panel");
    await row.first().click();
    const dlg = page.getByRole("dialog");
    await visible(dlg.getByText("# Report v1"), "the file preview");
    await visible(dlg.getByRole("heading", { name: "report.md" }), "preview title");
    const files = await api(`/threads/${sessionState.w}/files?path=`);
    if (!files.some((f) => f.name === "report.md")) throw new Error(`files API lacks report.md: ${JSON.stringify(files)}`);
    await page.keyboard.press("Escape");
    await gone(dlg, "preview closed");
  });

  // d1. approval -> approve
  await step("d1. a command needing approval waits, Approve runs it", async () => {
    const id = await newSession("E2E approve");
    await send("please run the script");
    const card = page.getByRole("alertdialog", { name: "Approval needed" });
    await visible(card, "the approval card");
    await visible(card.getByText("execute_command"), "approval names the tool");
    await visible(card.getByText(/node -e .*ran\.txt/), "approval shows the command");
    const th = await thread(id);
    eq(th.pendingApproval?.toolName, "execute_command", "server-side pendingApproval");
    if (findFile("ran.txt")) throw new Error("the command ran before the approval");
    await card.getByRole("button", { name: "Approve", exact: true }).click();
    await visible(page.getByText("The script finished."), "the final answer after approval");
    await gone(card, "approval card gone");
    await poll(() => findFile("ran.txt"), "ran.txt written by the approved command");
    const t = await idle(id).then(() => thread(id));
    eq(t.pendingApproval ?? null, null, "no approval pending");
    eq(roles(t).at(-1), "assistant", "run ended with an assistant message");
  });

  // d2. approval -> deny
  await step("d2. Deny keeps the command from running and the conversation continues", async () => {
    sessionState.d = await newSession("E2E deny");
    const id = sessionState.d;
    await send("please run the risky script");
    const card = page.getByRole("alertdialog", { name: "Approval needed" });
    await visible(card, "the approval card");
    await visible(card.getByText(/node -e .*denied\.txt/), "approval shows the command");
    await card.getByRole("button", { name: "Deny", exact: true }).click();
    await visible(page.getByText("Understood, I did not run it."), "the answer after denial");
    await gone(card, "approval card gone");
    await idle(id);
    if (findFile("denied.txt")) throw new Error("the denied command ran");
    // the conversation continues gracefully: another message gets an answer
    await send("hello again");
    await visible(page.getByText("Hello from the fake model."), "the answer to the follow-up message");
    await idle(id);
    const t = await thread(id);
    eq(roles(t).filter((r) => r === "user").length, 2, "two user messages stored");
    if (findFile("denied.txt")) throw new Error("the denied command ran later");
  });

  // e. reload renders the same transcript from history
  await step("e. reloading the page renders the transcript again", async () => {
    await page.goto(`${base}/#/t/${sessionState.w}`);
    await page.reload();
    await visible(page.getByText("please write a file", { exact: true }), "user message after reload");
    await page.getByRole("button", { name: /Worked/ }).first().click();
    await visible(page.getByRole("button", { name: /write_file/ }), "tool card after reload");
    await visible(page.getByText("I wrote report.md and nothing else."), "answer after reload");
    await visible(page.getByRole("treeitem").filter({ hasText: "report.md" }), "file after reload");
    // the transcript shows each message once
    eq(await page.getByText("I wrote report.md and nothing else.").count(), 1, "answer rendered once");
    eq(await page.getByRole("button", { name: /write_file/ }).count(), 1, "tool card rendered once");
    // and the deny conversation, with its tool card and both answers
    await page.goto(`${base}/#/t/${sessionState.d}`);
    await page.reload();
    await visible(page.getByText("Understood, I did not run it."), "denied answer after reload");
    await visible(page.getByText("Hello from the fake model."), "follow-up answer after reload");
    await page.getByRole("button", { name: /Worked/ }).first().click();
    await visible(page.getByRole("button", { name: /execute_command/ }), "execute_command card after reload");
  });

  // f. unknown thread id
  await step("f. an unknown thread id shows an error state", async () => {
    const r = await fetch(`${base}/api/threads/does-not-exist`);
    eq(r.status, 404, "API status for an unknown thread");
    const before = problems.length;
    await page.goto(`${base}/#/t/does-not-exist`);
    await visible(page.getByText(/not found/i), "a not-found message");
    // The browser itself logs "Failed to load resource: 404" for the failed request: inherent to the API call,
    // not an app error. It is the only entry tolerated, and only for this very URL.
    const extra = problems.splice(before).filter((p) => !/Failed to load resource: the server responded with a status of 404/.test(p));
    if (extra.length) problems.push(...extra);
  });

  // ---- the product surfaces: requirements, roles, tasks, the inbox ---------------------------------------------------
  // The Schedules and Usage pages are not in this tree yet, so they have no scenario here.
  const proj = {};
  const projectTab = async (tab) => {
    await page.goto(`${base}/#/p/${proj.id}?tab=${tab}`);
    await page.reload(); // a hash-only navigation does not reload: make sure the tab renders from its own queries
  };

  await step("g1. a project: Requirements tab creates, filters and changes the status of a requirement", async () => {
    proj.id = (await api("/projects", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ name: "E2E product" }) })).id;
    await projectTab("requirements");
    await page.getByRole("button", { name: /New requirement/ }).click();
    const dlg = page.getByRole("dialog");
    await dlg.getByLabel("Title").fill("Export the report as CSV");
    await dlg.getByLabel("Description (markdown)").fill("Add a CSV export to the report page.");
    await dlg.getByLabel("Labels").fill("export, ui");
    await dlg.getByRole("button", { name: "Create" }).click();
    const list = page.getByRole("list", { name: "Requirements" });
    await visible(list.getByText("Export the report as CSV"), "the new requirement in the list");
    const stored = await api(`/projects/${proj.id}/requirements`);
    eq(stored.map((r) => [r.title, r.status, r.labels]), [["Export the report as CSV", "backlog", ["export", "ui"]]], "stored requirement");
    // the search narrows the list
    await page.getByLabel("Search requirements").fill("nothing like it");
    await gone(list.getByText("Export the report as CSV"), "the requirement filtered out");
    await page.getByLabel("Search requirements").fill("csv");
    await visible(list.getByText("Export the report as CSV"), "the requirement found again");
    // open it and mark it ready
    await list.getByText("Export the report as CSV").click();
    const detail = page.getByRole("region", { name: "Requirement detail" });
    await visible(detail, "the requirement detail");
    await api(`/projects/${proj.id}/requirements/${stored[0].id}`, { method: "PATCH", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ status: "ready" }) });
    await poll(async () => (await api(`/projects/${proj.id}/requirements/${stored[0].id}`)).requirement.status === "ready", "the requirement is ready");
  });

  await step("g2. a role made in the Roles tab shows up in the composer's @ picker", async () => {
    await projectTab("roles");
    await page.getByRole("button", { name: /New role/ }).click();
    const dlg = page.getByRole("dialog");
    await dlg.getByLabel("Name").fill("docs-writer");
    await dlg.getByLabel("Title").fill("Docs writer");
    await dlg.getByLabel("Description").fill("Writes documentation");
    await dlg.getByLabel("Instructions").fill("You write clear documentation. Never touch source code.");
    await dlg.getByRole("button", { name: "Create" }).click();
    await gone(page.getByRole("dialog"), "the role editor closed");
    await visible(page.getByRole("list", { name: "Roles" }).getByText("@docs-writer"), "the role in the list");
    eq((await api(`/projects/${proj.id}/roles`)).some((r) => r.name === "docs-writer" && r.scope === "project"), true, "stored role");
    // a session of the project: typing @ offers the role
    const th = await api("/threads", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ title: "E2E roles", projectId: proj.id }) });
    await page.goto(`${base}/#/t/${th.id}`);
    await visible(composer, "composer");
    await composer.fill("@doc");
    const picker = page.getByRole("listbox", { name: "Mentions" });
    await visible(picker.getByText("@docs-writer"), "the role in the picker");
    await picker.getByRole("option").filter({ hasText: "@docs-writer" }).first().click();
    await visible(page.getByRole("button", { name: "Remove docs-writer" }), "the role as a chip");
    // an empty message with the chip is not sent; a message is, and the answer comes back
    await composer.fill("hello role");
    await page.getByRole("button", { name: "Send", exact: true }).click();
    await visible(page.getByText("Hello from the fake model."), "the answer of the run as the role");
    await idle(th.id);
    const t = await thread(th.id);
    eq(roles(t).filter((r) => r === "user").length, 1, "one user message stored");
  });

  await step("g3. the Tasks tab: a task is created, runs as an agent and finishes", async () => {
    await projectTab("tasks");
    await page.getByRole("button", { name: /New task/ }).click();
    const dlg = page.getByRole("dialog");
    await dlg.getByLabel("Title").fill("Say hello");
    await dlg.getByLabel("Spec").fill("Greet the team in one sentence.");
    await dlg.getByRole("button", { name: "Create task" }).click();
    await gone(page.getByRole("dialog"), "the task form closed");
    const main = page.getByRole("main");
    await visible(main.getByText("Say hello", { exact: true }), "the task in the list");
    const done = await poll(async () => {
      const ts = await api(`/projects/${proj.id}/tasks?limit=50`);
      const t = ts.find((x) => x.title === "Say hello");
      return t && ["done", "blocked", "failed", "review"].includes(t.state) ? t : null;
    }, "the task reaches an end state", 60_000);
    eq(done.state, "done", `task end state (${done.blockedReason ?? ""})`);
    // the state filter and the detail dialog show it
    await page.getByRole("group", { name: "Filter by state" }).getByRole("button", { name: /^Done/ }).click();
    await main.getByText("Say hello", { exact: true }).click();
    await visible(page.getByRole("region", { name: "Task detail" }).getByRole("link", { name: /agent's session/ }), "the link to the agent's session");
  });

  await step("g4. the inbox lists a session that waits for an approval and clears when it is approved", async () => {
    await page.goto(`${base}/#/inbox`);
    await visible(page.getByRole("main").getByText("Nothing waits for you"), "the empty inbox");
    const id = await newSession("E2E inbox");
    await send("please run the script");
    await visible(page.getByRole("alertdialog", { name: "Approval needed" }), "the approval card");
    await page.goto(`${base}/#/inbox`);
    await visible(page.getByRole("main").getByText("E2E inbox"), "the waiting session in the inbox");
    eq(await page.getByRole("main").getByText("Nothing waits for you").count(), 0, "the inbox is not empty");
    await page.getByRole("main").getByText("E2E inbox").first().click();
    await page.waitForURL(new RegExp(`#/t/${id}$`));
    const card = page.getByRole("alertdialog", { name: "Approval needed" });
    await visible(card, "the approval card after opening the session");
    await card.getByRole("button", { name: "Approve", exact: true }).click();
    await visible(page.getByText("The script finished."), "the answer after approval");
    await idle(id);
    await page.goto(`${base}/#/inbox`);
    await visible(page.getByRole("main").getByText("Nothing waits for you"), "the inbox is empty again");
  });

  await step("g5. Admin registers a git host; Profile, Git access takes an SSH key for it; the host's trust is enforced", async () => {
    eq((await api("/git-hosts")).enabled, true, "git hosts are on with a secret key");
    await page.goto(`${base}/#/admin`);
    await page.reload();
    await page.getByRole("tab", { name: "Git hosts" }).click();
    await page.getByRole("button", { name: /Add a git host/ }).click();
    await page.getByLabel("Host name").fill("git.e2e.example.com");
    await page.getByLabel("Kind").selectOption("gitea");
    await page.getByLabel("SSH host keys").fill("git.e2e.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIE2E");
    await page.getByRole("button", { name: "Add host" }).click();
    const row = page.getByRole("listitem", { name: "git.e2e.example.com" });
    await visible(row, "the registered host");
    await visible(row.getByText("1 SSH key(s)"), "its SSH host key count");
    eq((await api("/git-hosts")).hosts.map((h) => [h.name, h.kind, h.knownHosts.length]), [["git.e2e.example.com", "gitea", 1]], "stored host");
    // the user's side: Profile lists the host and generates a key; the private half is never returned
    await page.goto(`${base}/#/profile`);
    await page.reload();
    const mine = page.getByRole("listitem", { name: "git.e2e.example.com" });
    await visible(mine, "the host under Git access");
    await mine.getByRole("button", { name: /Generate an SSH key/ }).click();
    const pub = mine.getByLabel("Public key for git.e2e.example.com");
    await visible(pub, "the public key");
    const key = await pub.inputValue();
    if (!key.startsWith("ssh-ed25519 ")) throw new Error(`public key = ${key}`);
    const view = JSON.stringify(await api("/git-hosts"));
    if (view.includes("PRIVATE KEY")) throw new Error("the private key is in the API answer");
    eq((await api("/git-hosts")).hosts[0].credential.hasSshKey, true, "the key is stored");
    // a host nobody registered takes no credentials (the egress guard of the credentials)
    const r = await fetch(`${base}/api/me/git-hosts/evil.example.com/token`, { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ token: "x" }) });
    eq(r.status, 404, "an unregistered host takes no token");
    // remove it again from the admin page
    await page.goto(`${base}/#/admin`);
    await page.reload();
    await page.getByRole("tab", { name: "Git hosts" }).click();
    await page.getByRole("listitem", { name: "git.e2e.example.com" }).getByRole("button", { name: /Remove/ }).click();
    await gone(page.getByRole("listitem", { name: "git.e2e.example.com" }), "the host removed");
    eq((await api("/git-hosts")).hosts, [], "no host left");
  });

  // ---- h: workflows, schedules and usage, executors, the chat workbench, a route sweep -----------------------------
  const post = (p, body, method = "POST") => api(p, { method, headers: { "Content-Type": "application/json" }, body: JSON.stringify(body ?? {}) });
  // The browser logs "Failed to load resource: ... status of N" for a request that failed ON PURPOSE (a refused run, an invalid
  // expression). Only those statuses, only from `since` on, are tolerated; anything else stays a problem.
  const expectStatus = (since, ...codes) => {
    const extra = problems.splice(since).filter((p) => !codes.some((c) => p.includes(`Failed to load resource: the server responded with a status of ${c}`)));
    problems.push(...extra);
  };
  const toastText = (re) => page.locator("[data-sonner-toast]").filter({ hasText: re });

  await step("h1. Workflows: template -> designer -> cycle is refused -> publish -> run -> the gate in the inbox is approved", async () => {
    const hp = (await post("/projects", { name: "E2E workflows" })).id;
    proj.wf = hp;
    await page.goto(`${base}/#/p/${hp}?tab=workflows`);
    await page.reload();
    await page.getByRole("button", { name: "Use the Product proposal template" }).click();
    // the copy opens in the designer: three steps on the canvas
    const canvas = page.getByRole("group", { name: "Workflow canvas" });
    await visible(canvas.getByRole("button", { name: "Step Research" }), "the Research step on the canvas");
    await visible(canvas.getByRole("button", { name: "Step Draft the proposal" }), "the Draft step");
    await visible(canvas.getByRole("button", { name: "Step Decision" }), "the Decision gate");
    eq(await canvas.getByRole("button", { name: /^Connection from/ }).count(), 2, "two edges on the canvas");
    // edit a step's prompt in the inspector
    await canvas.getByRole("button", { name: "Step Research" }).click();
    const inspector = page.getByRole("complementary", { name: "Inspector" });
    const prompt = inspector.getByLabel("Prompt", { exact: true });
    await prompt.fill("Research briefly: {{inputs.topic}}");
    await visible(page.getByRole("button", { name: "Save", exact: true }).and(page.locator(":enabled")), "Save enabled once the prompt changed");
    // a cycle: Decision cannot be waited for by Research
    await inspector.getByRole("button", { name: /^Remove step/ }).waitFor();
    await canvas.getByRole("button", { name: "Connect from approve" }).click();
    await canvas.getByRole("button", { name: "Step Research" }).click();
    await visible(toastText(/wait for each other/), "the cycle is refused with a message");
    // an invalid graph (an empty prompt) shows in Problems and blocks nothing until fixed; fix it
    await prompt.fill("");
    await visible(page.getByRole("region", { name: "Problems" }).getByText("The agent needs a prompt."), "the validation message");
    await prompt.fill("Research briefly: {{inputs.topic}}");
    await visible(page.getByText("No problems found in the graph."), "problems cleared");
    // publish (saves first)
    await page.getByRole("button", { name: "Publish", exact: true }).click();
    await visible(page.getByText(/^published v\d+$/), "the published badge");
    const wfs = await api(`/projects/${hp}/workflows`);
    eq([wfs.length, wfs[0].status, wfs[0].nodes.find((n) => n.id === "research").prompt], [1, "published", "Research briefly: {{inputs.topic}}"], "stored published workflow");
    // run it
    await page.getByRole("button", { name: /Workflows$/ }).click();
    await page.getByRole("button", { name: "Run", exact: true }).click();
    const dlg = page.getByRole("dialog");
    await dlg.getByLabel("What to propose").fill("Add a dark mode");
    await dlg.getByRole("button", { name: /Start the run/ }).click();
    const runCanvas = page.getByRole("group", { name: "Workflow canvas" });
    await visible(runCanvas, "the run view");
    // the steps change status: the first two finish (answered by the fake LLM), the gate waits for a decision
    await poll(async () => (await runCanvas.locator('[data-kind="agent"][data-state="done"]').count()) === 2, "both agent steps done", 90_000);
    await visible(runCanvas.locator('[data-kind="gate"][data-state="waiting"]'), "the gate waits for a decision", 30_000);
    // the gate is in the inbox
    await page.goto(`${base}/#/inbox`);
    await visible(page.getByRole("main").getByText("Workflow gates (1)"), "the gate in the inbox");
    await page.getByRole("main").getByRole("button", { name: "Approve", exact: true }).click();
    await visible(page.getByRole("main").getByText("Nothing waits for you"), "the inbox is empty after approving");
    const runs = await api(`/projects/${hp}/workflow-runs`);
    await poll(async () => (await api(`/projects/${hp}/workflow-runs`))[0].state === "done", "the run is done", 30_000);
    eq(runs.length, 1, "one run");
    await page.goto(`${base}/#/p/${hp}?tab=workflows&run=${runs[0].id}`);
    await page.reload();
    await visible(page.getByRole("group", { name: "Workflow canvas" }).locator('[data-kind="gate"][data-state="done"]'), "the gate step is done in the run view");
  });

  await step("h2. Schedules and Usage: create, preview, toggle, run now, usage totals, the cap and a run refused over it", async () => {
    const since = problems.length;
    const hp = (await post("/projects", { name: "E2E schedules" })).id;
    await page.goto(`${base}/#/p/${hp}?tab=schedules`);
    await page.reload();
    await page.getByRole("button", { name: /New schedule/ }).click();
    const dlg = page.getByRole("dialog");
    await dlg.getByLabel("Name").fill("Quarter-hour hello");
    await dlg.getByLabel("Prompt").fill("Say hello to the team.");
    await dlg.getByRole("combobox", { name: "Repeat" }).click();
    await page.getByRole("option", { name: "Every 15 minutes" }).click();
    eq(await dlg.getByLabel("Cron or interval").inputValue(), "*/15 * * * *", "the preset fills the cron field");
    await dlg.getByLabel("Time zone").fill("Asia/Tokyo");
    const next = dlg.getByRole("list", { name: "Next runs" });
    await visible(next.getByRole("listitem"), "the next fire times");
    await visible(dlg.getByText(/Next \d+ runs \(Asia\/Tokyo\)/), "the preview names the zone");
    if ((await next.getByRole("listitem").count()) < 3) throw new Error("fewer than 3 next fire times");
    // an invalid expression shows the server's message instead of times
    await dlg.getByLabel("Cron or interval").fill("not a cron");
    await visible(dlg.getByRole("alert"), "the preview error for a bad expression");
    await dlg.getByLabel("Cron or interval").fill("*/15 * * * *");
    await visible(next.getByRole("listitem"), "times again");
    await dlg.getByRole("button", { name: "Create" }).click();
    await gone(page.getByRole("dialog"), "the schedule form closed");
    const list = page.getByRole("list", { name: "Schedules" });
    await visible(list.getByText("Quarter-hour hello"), "the schedule in the list");
    const sch = (await api(`/projects/${hp}/schedules`))[0];
    eq([sch.cron, sch.timezone, sch.enabled], ["*/15 * * * *", "Asia/Tokyo", true], "stored schedule");
    // toggle enabled
    const sw = list.getByRole("switch", { name: "Enable Quarter-hour hello" });
    await sw.click();
    await visible(list.getByText("paused"), "the schedule shows paused");
    eq((await api(`/projects/${hp}/schedules`))[0].enabled, false, "stored disabled");
    await sw.click();
    await poll(async () => (await api(`/projects/${hp}/schedules`))[0].enabled === true, "enabled again");
    // run now, and the run in the history links to a session
    await list.getByRole("button", { name: "Run Quarter-hour hello now" }).click();
    await list.getByRole("button", { name: "Show runs of Quarter-hour hello" }).click();
    const hist = list.getByRole("table", { name: "Runs of Quarter-hour hello" });
    await visible(hist.getByText("succeeded"), "the manual run succeeded", 60_000);
    await visible(hist.getByText("manual"), "the run is marked manual");
    const link = hist.getByRole("link", { name: "Open session" });
    await link.click();
    await page.waitForURL(/#\/t\/[^/]+$/);
    await visible(page.getByText("Hello from the fake model."), "the scheduled run's answer in its session");
    // Usage: the personal page and the project tab show totals now that a run exists
    await page.goto(`${base}/#/usage`);
    await page.reload();
    await visible(page.getByRole("heading", { name: "Usage" }), "the usage page");
    await visible(page.getByText("Scheduled tasks").locator("xpath=..").getByText(/1 runs/), "one scheduled run counted");
    await visible(page.getByLabel("By kind of work").getByText(/^\$0\.0/).first(), "a scheduled-task cost");
    await page.goto(`${base}/#/p/${hp}?tab=usage`);
    await page.reload();
    const costTile = page.getByText("Cost", { exact: true }).locator("xpath=..");
    await visible(costTile.getByText(/^\$0\.0\d+$/), "the project cost total");
    await visible(page.getByRole("table", { name: /Usage by day/ }), "the by-day table");
    await visible(page.getByText(/no cap/), "no cap yet");
    // the owner sets a cap: the bar and the reached warning show
    await page.getByLabel(/^Cap \(USD per month/).fill("0.0001");
    await page.getByRole("button", { name: "Save cap" }).click();
    await visible(page.getByRole("progressbar", { name: "Share of the monthly cap spent" }), "the cap bar");
    await visible(page.getByRole("alert").getByText(/The cap is used up/), "the cap-reached notice");
    // a new run in the project is refused, with the reason on screen
    const th = await post("/threads", { title: "E2E over the cap", projectId: hp });
    await page.goto(`${base}/#/t/${th.id}`);
    await visible(composer, "composer");
    await send("hello over the cap");
    await visible(page.getByText(/cap/i).filter({ hasText: /spend|monthly|reached|used up/i }), "the refusal is visible");
    eq(((await thread(th.id)).messages ?? []).some((m) => m.role === "assistant"), false, "no answer was produced");
    expectStatus(since, 400, 402);
    // raising the cap lets the run start again
    await post(`/projects/${hp}/usage/cap`, { monthlyUsd: 0 }, "PUT");
    await send("hello after the cap is lifted");
    await visible(page.getByText("Hello from the fake model."), "the answer once the cap is lifted");
  });

  await step("h3. Executors: a project executor shows in the role editor's picker; a credential is saved and never shown", async () => {
    const hp = (await post("/projects", { name: "E2E executors" })).id;
    await page.goto(`${base}/#/p/${hp}?tab=roles`);
    await page.reload();
    const section = page.getByRole("region", { name: "Executors" });
    await visible(section, "the Executors section");
    await section.getByRole("button", { name: /New executor/ }).click();
    const dlg = page.getByRole("dialog");
    await dlg.getByLabel("Name", { exact: true }).fill("my-agent");
    await dlg.getByLabel("Title").fill("My agent");
    await dlg.getByLabel("Command", { exact: true }).fill("my-agent run {task}");
    await dlg.getByLabel("Program").fill("my-agent");
    await dlg.getByLabel("Credential variables").fill("MY_AGENT_API_KEY");
    await dlg.getByRole("button", { name: "Create" }).click();
    await gone(page.getByRole("dialog"), "the executor form closed");
    const mine = section.getByRole("list", { name: "Project executors" }).getByRole("listitem").filter({ hasText: "my-agent" });
    await visible(mine.getByText("Project", { exact: true }), "the executor is listed as a project one");
    eq((await api(`/projects/${hp}/executors`)).some((e) => e.name === "my-agent" && e.kind === "custom"), true, "stored executor");
    // an invalid draft is explained instead of created
    await section.getByRole("button", { name: /New executor/ }).click();
    await page.getByRole("dialog").getByLabel("Name", { exact: true }).fill("bad");
    await page.getByRole("dialog").getByLabel("Command", { exact: true }).fill("no placeholder");
    await visible(page.getByRole("dialog").getByRole("alert").getByText(/must contain \{task\}/), "the validation message");
    await page.getByRole("dialog").getByRole("button", { name: "Cancel" }).click();
    // the role editor offers it
    await page.getByRole("button", { name: /New role/ }).click();
    const rd = page.getByRole("dialog");
    await rd.getByLabel("Name").fill("delegator");
    await rd.getByLabel("Instructions").fill("Delegate coding to the external agent.");
    await rd.getByRole("combobox", { name: "Executor" }).click();
    await visible(page.getByRole("option", { name: "My agent (project)" }), "the project executor in the picker");
    await page.getByRole("option", { name: "My agent (project)" }).click();
    await rd.getByRole("button", { name: "Create" }).click();
    await gone(page.getByRole("dialog"), "the role editor closed");
    await visible(page.getByRole("list", { name: "Roles" }).getByText("runs on my-agent"), "the role runs on the executor");
    eq((await api(`/projects/${hp}/roles`)).find((r) => r.name === "delegator")?.executor, "my-agent", "stored role executor");

    // Settings: a builtin executor's credential is write-only
    const builtin = (await api("/executors")).find((e) => e.credentialEnv?.length);
    if (!builtin) throw new Error("no builtin executor takes credentials");
    await page.goto(`${base}/#/profile`);
    await page.reload();
    const card = page.getByRole("list", { name: "Executors" });
    const line = card.getByRole("listitem").filter({ has: page.locator(`code:text-is("${builtin.name}")`) });
    await line.getByRole("button", { name: `Credentials for ${builtin.name}` }).click();
    const secret = "sk-e2e-very-secret-value";
    const field = line.getByLabel(builtin.credentialEnv.find((n) => !n.endsWith("_URL")) ?? builtin.credentialEnv[0], { exact: true });
    await field.fill(secret);
    await line.getByRole("button", { name: "Save", exact: true }).click();
    await visible(line.getByText("credentials set"), "the executor shows as configured");
    eq((await api("/executors")).find((e) => e.name === builtin.name).configured, true, "stored as configured");
    // the value is nowhere: not in the API answer, not in the page, and the form shows a placeholder only
    if (JSON.stringify(await api("/executors")).includes(secret)) throw new Error("the credential is in the API answer");
    if ((await page.content()).includes(secret)) throw new Error("the credential is in the page");
    await line.getByRole("button", { name: `Credentials for ${builtin.name}` }).click();
    eq(await line.getByLabel(builtin.credentialEnv.find((n) => !n.endsWith("_URL")) ?? builtin.credentialEnv[0], { exact: true }).inputValue(), "", "the field is empty again");
    await visible(line.getByPlaceholder("set: leave empty to keep it"), "the field says it is set");
  });

  await step("h4. Chat workbench: the queue while a reply runs, an inline artifact card, the Files tab, one environment", async () => {
    const id = await newSession("E2E workbench");
    await send("slow reply please");
    await fake.held();
    await visible(page.getByText(/Slow reply finally/), "the partial reply while the run is held");
    // two messages sent meanwhile go into the queue card
    for (const text of ["queued beta message", "queued alpha message"]) {
      await composer.fill(text);
      await page.getByRole("button", { name: "Queue", exact: true }).click(); // the Send button reads "Queue" while a reply runs
    }
    const queue = page.getByRole("region", { name: "Queued messages" });
    await visible(queue.getByText("2 messages queued"), "two queued messages");
    await visible(queue.getByText("queued beta message"), "the first queued message");
    // Remove drops one of them (server side too)
    await queue.getByRole("button", { name: "Remove: queued beta message" }).click();
    await visible(queue.getByText("1 message queued"), "one message left");
    eq((await api(`/threads/${id}/queue`)).map((q) => q.text), ["queued alpha message"], "the server queue");
    // the reply completes and the remaining message is sent by itself
    fake.release();
    await visible(page.getByText("Slow reply finally done."), "the held reply completes");
    await visible(page.getByText("Alpha answer."), "the queued message was sent and answered");
    await gone(queue, "the queue card is gone");
    eq((await api(`/threads/${id}/queue`)).length, 0, "the queue is empty");
    eq(await page.getByText("Beta answer.").count(), 0, "the removed message was never sent");
    await idle(id);
    eq((await thread(id)).messages.filter((m) => m.role === "user").map((m) => m.content), ["slow reply please", "queued alpha message"], "the stored user messages");
    // one environment: no selector
    eq(await page.getByLabel("Runtime environment").count(), 0, "the environment control is hidden with one environment");

    // a published artifact is a card in the transcript, with preview and download
    await send("please publish the page");
    const card = page.getByRole("figure", { name: "Artifact E2E page" });
    await visible(card, "the artifact card");
    await visible(card.getByRole("tab", { name: "preview" }), "the preview tab");
    const frame = card.frameLocator("iframe");
    await visible(frame.getByRole("heading", { name: "E2E page" }), "the page rendered in the card's preview");
    await card.getByRole("tab", { name: "source" }).click();
    await visible(card.getByText("published by the agent"), "the source view");
    const [dl] = await Promise.all([page.waitForEvent("download"), card.getByRole("button", { name: /Download/ }).click()]);
    eq(dl.suggestedFilename(), "page.html", "the download's file name");
    eq(fs.readFileSync(await dl.path(), "utf8").includes("published by the agent"), true, "the downloaded content");
    await visible(page.getByText("Published the page."), "the closing text");
    // and it is listed in the Files tab
    const list = page.getByRole("region", { name: "Artifacts" });
    await visible(list.getByText("E2E page"), "the artifact in the Files tab");
    eq((await api(`/threads/${id}/published`)).map((a) => a.name), ["page.html"], "the published list");
    await list.getByRole("button", { name: /E2E page/ }).click();
    await visible(page.getByRole("dialog").getByRole("heading", { name: "E2E page" }), "the artifact opens from the list");
    await page.keyboard.press("Escape");
    await idle(id);
  });

  await step("h5. Route sweep: every top-level route and project tab renders without errors, on desktop and on a phone", async () => {
    const hp = (await post("/projects", { name: "E2E sweep" })).id;
    const top = ["/", "/supervisor", "/inbox", "/jobs", "/references", "/connectors", "/capabilities", "/schedules", "/roles", "/usage", "/insights", "/memory", "/profile", "/admin"];
    const tabs = ["board", "overview", "members", "memory", "capabilities", "roles", "schedules", "recipes", "usage", "insights", "notifications", "audit", "tasks", "requirements", "workflows", "settings"];
    const failures = [];
    const bad = [/Something went wrong/, /^Not found$/, /You do not have access to this/, /Try again/];
    const sweep = async (pg, label) => {
      let where = "";
      pg.on("response", (r) => r.status() >= 400 && failures.push(`${label} ${where}: ${r.status()} ${r.request().method()} ${r.url().replace(base, "")}`));
      const check = async (url, what, tab) => {
        where = what;
        await pg.goto(url);
        // (networkidle never comes: the live feed keeps a stream open.) Settled = two animation frames after the route rendered,
        // with no loading skeleton or spinner left.
        await pg.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));
        await pg.waitForFunction(() => !document.querySelector('[data-slot="skeleton"], [aria-busy="true"], [aria-label="Loading"]'), null, { timeout: T });
        // a tab is not rendered until its trigger is selected
        if (tab) await pg.getByRole("tab", { selected: true }).first().waitFor({ state: "visible", timeout: T });
        await pg.locator("main, body").first().waitFor({ state: "visible", timeout: T });
        for (const re of bad) if ((await pg.getByText(re).count()) > 0) failures.push(`${label} ${what}: shows ${re}`);
        const toasts = await pg.locator('[data-sonner-toast][data-type="error"]').allInnerTexts();
        if (toasts.length) failures.push(`${label} ${what}: error toast ${JSON.stringify(toasts)}`);
        if (tab) {
          const selected = (await pg.getByRole("tab", { selected: true }).first().textContent())?.trim().toLowerCase();
          if (!selected) failures.push(`${label} ${what}: no tab is selected`);
        }
        if (((await pg.locator("body").innerText()) ?? "").trim().length < 20) failures.push(`${label} ${what}: the page is blank`);
        // no sideways page scroll on a phone (the tab strip and wide tables scroll inside their own box)
        const overflow = await pg.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
        if (overflow > 0) failures.push(`${label} ${what}: the page scrolls sideways by ${overflow}px`);
      };
      for (const r of top) await check(`${base}/#${r}`, r, false);
      for (const t of tabs) await check(`${base}/#/p/${hp}?tab=${t}`, `project tab ${t}`, true);
    };
    await sweep(page, "desktop");
    const phone = await browser.newContext({ viewport: { width: 390, height: 844 } });
    phone.on("page", watch);
    await phone.addInitScript(onboarded);
    try {
      await sweep(await phone.newPage(), "phone 390x844");
    } finally {
      await phone.close();
    }
    if (failures.length) throw new Error(failures.join("\n"));
  });

  await step("h6. a session card on the dashboard opens its session when clicked anywhere, not only on the title", async () => {
    const hp = (await post("/projects", { name: "E2E cards" })).id;
    const th = await post("/threads", { title: "E2E clickable card", projectId: hp });
    await page.goto(`${base}/#/`);
    const card = page.locator("div", { has: page.getByRole("link", { name: "E2E clickable card" }) }).filter({ hasText: "Last activity" }).last();
    await visible(card, "the session's card");
    // A real click on the card's own text, away from the title link. The link's overlay covers the text, so
    // playwright's element click (which refuses covered targets) is not the way: click the point, as a person does.
    const box = await card.getByText(/^Last activity/).boundingBox();
    if (!box) throw new Error("the card text has no box");
    await page.mouse.click(box.x + box.width / 2, box.y + box.height / 2);
    await poll(() => page.url().endsWith(`#/t/${th.id}`), "the card click to open the session");
  });

  await step("j1. Insights on a fresh project explains that numbers appear after the first merged task", async () => {
    const jp = (await post("/projects", { name: "E2E insights empty" })).id;
    await page.goto(`${base}/#/p/${jp}?tab=insights`);
    await page.reload();
    await visible(page.getByText("Insights appear after the first merged task"), "the empty state");
    const link = page.getByRole("link", { name: "Hand off a first task" }).filter({ visible: true }).first();
    await visible(link, "the link to hand off a first task");
    await link.click();
    await poll(() => page.url().includes("tab=tasks"), "the link opens the Tasks tab");
  });

  await step("j2. Insights after a task ran: summary, headline numbers and charts render", async () => {
    await page.goto(`${base}/#/p/${proj.id}?tab=insights`);
    await page.reload();
    const main = page.getByRole("main");
    const summary = main.getByTestId("insights-summary").filter({ visible: true }).first();
    await visible(summary, "the plain-language summary");
    const text = await summary.innerText();
    if (!/started \d+ tasks?/.test(text) && !/merged \d+ PR/.test(text)) throw new Error(`unexpected summary: ${text}`);
    for (const label of ["Merged PRs", "First-pass success", "Cost per PR"]) await visible(main.getByText(label, { exact: true }), label);
    await visible(main.getByRole("img", { name: /Cost per day/ }), "the cost chart");
    await visible(main.getByRole("img", { name: /Merged PRs per week/ }), "the throughput chart");
    const ins = await api(`/projects/${proj.id}/insights?range=30d`);
    if (!(ins.cost.totalUsd > 0) || ins.delivery.started < 1) throw new Error(`insights numbers: ${JSON.stringify(ins.delivery)} ${JSON.stringify(ins.cost.totalUsd)}`);
    await main.getByRole("button", { name: "7 days" }).filter({ visible: true }).first().click();
    await poll(async () => (await api(`/projects/${proj.id}/insights?range=7d`)).days === 7, "a 7 day range");
    // a conversation's own cost line
    const tu = await api(`/threads/${sessionState.w}/usage`);
    if (!(tu.total.costUsd > 0)) throw new Error(`thread usage: ${JSON.stringify(tu)}`);
  });

  await step("j3. the downloaded report is a self-contained page that opens without errors or requests", async () => {
    const q = `range=30d&tz=UTC`;
    const html = await (await fetch(`${base}/api/projects/${proj.id}/insights/report?${q}&format=html`)).text();
    for (const bad of ["<script", "<link", "<img", "<iframe", "url(", "@import"]) if (html.toLowerCase().includes(bad)) throw new Error(`the report contains ${bad}`);
    if (/https?:\/\/(?!www\.w3\.org\/2000\/svg)/.test(html)) throw new Error("the report mentions an external URL");
    if (!html.includes("<svg")) throw new Error("the report has no charts");
    const csv = await (await fetch(`${base}/api/projects/${proj.id}/insights/report?${q}&format=csv`)).text();
    if (!csv.startsWith("id,title,role,state") || !csv.includes("Say hello")) throw new Error(`csv: ${csv.slice(0, 200)}`);
    const raw = await (await fetch(`${base}/api/projects/${proj.id}/insights/report?${q}&format=json`)).json();
    if (!raw.summary) throw new Error("json report without summary");
    // open the saved file in a fresh page: nothing but the document itself may be requested
    const file = path.join(tmp, "insights-report.html");
    fs.writeFileSync(file, html);
    const rp = await ctx.newPage();
    const requested = [];
    rp.on("request", (r) => requested.push(r.url()));
    const before = problems.length;
    await rp.goto(`file://${file}`);
    await rp.getByRole("heading", { name: /^Insights:/ }).waitFor({ state: "visible", timeout: T });
    if (requested.some((u) => !u.startsWith("file://"))) throw new Error(`the report requested ${requested.join(", ")}`);
    if ((await rp.locator("svg").count()) < 3) throw new Error("charts missing in the opened report");
    if (process.env.E2E_SHOTS) {
      fs.mkdirSync(process.env.E2E_SHOTS, { recursive: true });
      await rp.screenshot({ path: path.join(process.env.E2E_SHOTS, "insights-report.png"), fullPage: true });
    }
    if (problems.length > before) throw new Error(`console problems in the report: ${problems.slice(before).join("; ")}`);
    await rp.close();
    // and the app's own menu offers the downloads
    await page.goto(`${base}/#/p/${proj.id}?tab=insights`);
    await page.reload();
    await page.getByRole("button", { name: "Download report" }).filter({ visible: true }).first().click();
    await visible(page.getByRole("menuitem", { name: /Task table \(CSV\)/ }), "the CSV item");
    await page.keyboard.press("Escape");
  });

  // k. Recipes: the gallery, the wizard, sharing, importing, triggers
  const shotTo = async (pg, name) => {
    if (!process.env.E2E_SHOTS) return;
    fs.mkdirSync(process.env.E2E_SHOTS, { recursive: true });
    await pg.waitForTimeout(600);
    await pg.screenshot({ path: path.join(process.env.E2E_SHOTS, `${name}.png`) });
  };
  const rk = {};
  await step("k1. Recipes: open the gallery, filter it, apply 'Weekly project status digest' and find the schedule in Schedules", async () => {
    rk.a = (await post("/projects", { name: "E2E recipes A" })).id;
    await page.goto(`${base}/#/p/${rk.a}?tab=recipes`);
    await page.reload();
    const cards = page.getByRole("list", { name: "Recipes" });
    await visible(cards.getByRole("listitem").first(), "the gallery cards");
    if ((await cards.getByRole("listitem").count()) < 12) throw new Error("fewer than 12 recipes in the gallery");
    await shotTo(page, "recipes-gallery");
    await page.getByRole("group", { name: "Category" }).getByRole("button", { name: "Security" }).click();
    await visible(cards.getByRole("listitem", { name: "Nightly dependency and vulnerability scan" }), "a security recipe");
    await gone(cards.getByRole("listitem", { name: "Weekly project status digest" }), "a reporting recipe hidden by the security chip");
    await page.getByRole("group", { name: "Category" }).getByRole("button", { name: "All" }).click();
    await page.getByLabel("Search recipes").fill("status digest");
    const card = cards.getByRole("listitem", { name: "Weekly project status digest" });
    await visible(card, "the digest card after searching");
    await visible(card.getByText("A schedule: every Friday at 17:00 (UTC)"), "what it makes, in words");
    await card.getByRole("button", { name: "Use this recipe" }).click();
    const dlg = page.getByRole("dialog");
    await visible(dlg.getByText("This will create"), "the wizard preview");
    await dlg.getByLabel(/^Time zone/).fill("Europe/Paris");
    await visible(dlg.getByLabel("What the agent will be told").getByText(/status report/i), "the rendered prompt");
    await shotTo(page, "recipes-wizard");
    await dlg.getByRole("button", { name: "Create" }).click();
    await visible(dlg.getByText(/Done: Weekly project status digest/), "the done message");
    await dlg.getByRole("link", { name: "Open Schedules" }).click();
    const list = page.getByRole("list", { name: "Schedules" });
    await visible(list.getByText("Weekly status digest"), "the new schedule in Schedules");
    const sch = (await api(`/projects/${rk.a}/schedules`))[0];
    eq([sch.name, sch.cron, sch.timezone, sch.policy, sch.enabled], ["Weekly status digest", "0 17 * * 5", "Europe/Paris", "readonly", true], "the stored schedule");
    await shotTo(page, "recipes-schedules-tab");
    if (process.env.E2E_SHOTS) {
      const phone = await browser.newContext({ viewport: { width: 390, height: 844 } });
      await phone.addInitScript(onboarded);
      phone.on("page", watch);
      try {
        const pp = await phone.newPage();
        await pp.goto(`${base}/#/p/${rk.a}?tab=recipes`);
        await visible(pp.getByRole("list", { name: "Recipes" }).getByRole("listitem").first(), "the gallery on a phone");
        await shotTo(pp, "phone-recipes-gallery");
        await pp.getByRole("button", { name: "Use this recipe" }).first().click();
        await visible(pp.getByRole("dialog").getByText("This will create"), "the wizard on a phone");
        await shotTo(pp, "phone-recipes-wizard");
        await pp.keyboard.press("Escape");
        await pp.goto(`${base}/#/p/${rk.a}?tab=schedules`);
        await visible(pp.getByRole("button", { name: /Add from a recipe/ }), "the button on a phone");
        await shotTo(pp, "phone-schedules-tab");
      } finally {
        await phone.close();
      }
    }
  });

  await step("k2. Recipes: share a schedule as a recipe, then apply it in another project", async () => {
    rk.b = (await post("/projects", { name: "E2E recipes B" })).id;
    await post(`/projects/${rk.a}/schedules`, { name: "Merged PRs of billing", prompt: "Summarise the merged pull requests of billing for the team.", cron: "0 7 * * 1", timezone: "UTC", policy: "readonly" });
    await page.goto(`${base}/#/p/${rk.a}?tab=schedules`);
    await page.reload();
    await page.getByRole("button", { name: "Share Merged PRs of billing as a recipe" }).click();
    const dlg = page.getByRole("dialog");
    await dlg.getByLabel("Recipe name").fill("PR summary");
    await dlg.getByLabel("What it does, in one line").fill("Summarise last week's merged pull requests");
    const area = dlg.getByLabel("Text");
    await area.evaluate((el) => {
      const i = el.value.indexOf("billing");
      el.focus();
      el.setSelectionRange(i, i + "billing".length);
    });
    await area.dispatchEvent("mouseup");
    await dlg.getByRole("button", { name: /Make the selection a parameter/ }).click();
    await dlg.getByLabel("Label for billing").fill("Product name");
    await dlg.getByRole("button", { name: "Share", exact: true }).click();
    await gone(page.getByRole("dialog"), "the share dialog closed");
    const mine = (await api("/recipes")).find((r) => r.name === "PR summary");
    if (!mine) throw new Error("the shared recipe is not in the library");
    eq([mine.scope, mine.body.prompt], ["personal", "Summarise the merged pull requests of {{billing}} for the team."], "the shared recipe");
    // use it in project B with another value
    await page.goto(`${base}/#/p/${rk.b}?tab=recipes`);
    await page.reload();
    await page.getByLabel("Search recipes").fill("PR summary");
    await page.getByRole("list", { name: "Recipes" }).getByRole("listitem", { name: "PR summary" }).getByRole("button", { name: "Use this recipe" }).click();
    const wiz = page.getByRole("dialog");
    await wiz.getByLabel(/^Product name/).fill("checkout");
    await visible(wiz.getByLabel("What the agent will be told").getByText(/of checkout for the team/), "the live preview with the new value");
    await wiz.getByRole("button", { name: "Create" }).click();
    await visible(wiz.getByText(/Done: PR summary/), "applied in the other project");
    const made = (await api(`/projects/${rk.b}/schedules`))[0];
    eq([made.prompt, made.name], ["Summarise the merged pull requests of checkout for the team.", "Merged PRs of checkout"], "the schedule of project B");
  });

  await step("k3. Recipes: export a recipe, import it as a file, and a broken file is refused", async () => {
    const exported = await (await fetch(`${base}/api/recipes/daily-ci-triage/export`)).json();
    eq(exported.format, "7ar-recipe", "the export format");
    exported.recipe.name = "My CI triage";
    const good = path.join(tmp, "good.recipe.json");
    const bad = path.join(tmp, "bad.recipe.json");
    fs.writeFileSync(good, JSON.stringify(exported));
    fs.writeFileSync(bad, JSON.stringify({ ...exported, extra: true }));
    await page.goto(`${base}/#/p/${rk.a}?tab=recipes`);
    await page.reload();
    await page.getByRole("button", { name: /Import a file/ }).click();
    const dlg = page.getByRole("dialog");
    // a file with an unknown field is refused by the server (asked directly: the browser logs every 4xx as a console error)
    const refused = await fetch(`${base}/api/recipes/import?scope=personal`, { method: "POST", headers: { "Content-Type": "application/json" }, body: fs.readFileSync(bad) });
    eq(refused.status, 400, "an unknown field in a recipe file is refused");
    await dlg.getByLabel("Recipe file").setInputFiles(good);
    await gone(page.getByRole("dialog"), "the import dialog closed");
    await poll(async () => (await api("/recipes")).some((r) => r.name === "My CI triage" && r.scope === "personal"), "the imported recipe is stored");
    await page.getByRole("button", { name: "Shared by people" }).click();
    await visible(page.getByRole("list", { name: "Recipes" }).getByRole("listitem", { name: "My CI triage" }), "the imported recipe in the gallery");
  });

  await step("k4. Recipes: a trigger from a recipe, and the trigger form's Advanced fields", async () => {
    await page.goto(`${base}/#/p/${rk.a}?tab=tasks`);
    await page.reload();
    await page.getByRole("tab", { name: "Triggers", exact: true }).click();
    await page.getByRole("button", { name: /Add from a recipe/ }).click();
    const gallery = page.getByRole("dialog");
    await gallery.getByLabel("Search recipes").fill("dependency update");
    await gallery.getByRole("listitem", { name: "Weekly dependency update pull request" }).getByRole("button", { name: "Use this recipe" }).click();
    const wiz = page.getByRole("dialog");
    await wiz.getByRole("button", { name: "Create" }).click();
    await visible(wiz.getByText(/Done: Weekly dependency update/), "the trigger was created");
    await wiz.getByRole("link", { name: /Open Tasks/ }).click();
    const trigs = await api(`/projects/${rk.a}/triggers`);
    const t = trigs.find((x) => x.name === "Weekly dependency update");
    if (!t) throw new Error("the trigger is missing");
    eq([t.kind, t.schedule, t.template.policy, t.template.budget.maxUsd], ["cron", "0 6 * * 1", "sandboxed", 3], "the stored trigger");
    // the form: Advanced fields reach the stored trigger
    await page.getByRole("tab", { name: "Triggers", exact: true }).click();
    await visible(page.getByText("Weekly dependency update").first(), "the trigger in the list");
    await page.getByRole("button", { name: /New trigger/ }).click();
    const form = page.getByRole("dialog");
    await form.getByLabel("Name").fill("Nightly lint");
    await form.getByLabel("Schedule").fill("0 3 * * *");
    await form.getByLabel("Task title").fill("Lint");
    await form.getByRole("button", { name: "Advanced" }).click();
    await form.getByLabel("Budget per task (USD)").fill("1.5");
    await form.getByLabel("Base branch").fill("develop");
    await form.getByRole("button", { name: "Create trigger" }).click();
    await gone(page.getByRole("dialog"), "the trigger form closed");
    const made = (await api(`/projects/${rk.a}/triggers`)).find((x) => x.name === "Nightly lint");
    eq([made?.template.budget?.maxUsd, made?.template.baseBranch], [1.5, "develop"], "the trigger's Advanced fields");
  });

  // ---- l: notifications -------------------------------------------------------------------------------------------
  const lp = {};
  const signed = (c, secret) => c.headers["x-7ar-signature"] === "sha256=" + crypto.createHmac("sha256", secret).update(c.body).digest("hex");

  await step("l1. Notifications tab: add a webhook channel with the wizard, it sends a signed test message", async () => {
    lp.id = (await post("/projects", { name: "E2E notify" })).id;
    await page.goto(`${base}/#/p/${lp.id}?tab=notifications`);
    await page.reload();
    const main = page.getByRole("main");
    await visible(main.getByText("No channels yet"), "the empty state that explains channels");
    await main.getByRole("button", { name: /Add channel/ }).click();
    const dlg = page.getByRole("dialog");
    await dlg.getByText("Webhook", { exact: true }).click();
    await dlg.getByLabel("Name").fill("Ops hook");
    await dlg.getByLabel("Webhook address").fill(hook.url);
    await dlg.getByLabel("Signing secret (optional)").fill("e2e-signing-secret");
    await dlg.getByRole("button", { name: /Add and send a test/ }).click();
    await gone(page.getByRole("dialog"), "the wizard closed");
    await poll(() => hook.calls.length >= 1, "the test message at the receiver");
    const c = hook.calls[0];
    eq(signed(c, "e2e-signing-secret"), true, "a valid X-7ar-Signature");
    const body = JSON.parse(c.body);
    eq([body.event, body.project], ["test", "E2E notify"], "the payload names the event and project");
    // the address and the secret are never shown or returned
    const chans = await api(`/projects/${lp.id}/notification-channels`);
    eq(JSON.stringify(chans).includes("e2e-hook") || JSON.stringify(chans).includes("e2e-signing-secret"), false, "no address or secret in the API");
    await visible(main.getByText("Ops hook").first(), "the channel in the list");
    // the Test button on the row (the server allows one test a second per channel)
    await new Promise((r) => setTimeout(r, 1200));
    await main.getByRole("button", { name: /^Test$/ }).click();
    await visible(main.getByText(/Delivered: the test message arrived/), "the delivered result");
    // the log has it
    await visible(main.getByRole("table", { name: "Delivery log" }).getByText("delivered").first(), "a delivered entry in the log");
  });

  await step("l2. a failing receiver is shown as failed with its reason, and highlighted in the log", async () => {
    hook.code = 500;
    await new Promise((r) => setTimeout(r, 1200)); // one test a second per channel
    await page.getByRole("button", { name: /^Test$/ }).click();
    await visible(page.getByRole("main").getByText(/Failed: the server answered 500/), "the failed result");
    const row = page.getByRole("table", { name: "Delivery log" }).getByRole("row").filter({ hasText: "failed" }).first();
    await visible(row, "a failed row in the log");
    eq((await row.getAttribute("class")).includes("bg-destructive"), true, "the failed row is highlighted");
    await visible(page.getByText("Why did I not get a message?"), "the why-not hint");
    hook.code = 200;
  });

  await step("l3. a rule sends an approval that waits to the channel, and the delivery log shows it", async () => {
    const main = page.getByRole("main");
    await main.getByLabel("An approval is waiting: Ops hook").click(); // saved on the server, then shown ticked
    const rules = await poll(async () => {
      const v = await api(`/projects/${lp.id}/notifications`);
      return v.rules.events["approval.waiting"]?.channels?.length ? v.rules : null;
    }, "the rule saved");
    eq(rules.events["approval.waiting"].person, true, "the person stays on");
    const before = hook.calls.length;
    const th = await post("/threads", { title: "E2E notify approval", projectId: lp.id });
    await page.goto(`${base}/#/t/${th.id}`);
    await visible(composer, "composer");
    await send("please run the script");
    await visible(page.getByRole("alertdialog", { name: "Approval needed" }), "the approval card");
    await poll(() => hook.calls.length > before, "the approval event at the receiver");
    const got = hook.calls.slice(before).map((c) => JSON.parse(c.body)).find((b) => b.event === "approval.waiting");
    eq(Boolean(got), true, "an approval.waiting payload");
    await poll(async () => (await api(`/projects/${lp.id}/notifications/log`)).some((e) => e.event === "approval.waiting" && e.status === "delivered"), "the log entry");
    await page.getByRole("alertdialog", { name: "Approval needed" }).getByRole("button", { name: "Approve", exact: true }).click();
    await visible(page.getByText("The script finished."), "the answer after approval");
    await idle(th.id);
    await page.goto(`${base}/#/p/${lp.id}?tab=notifications`);
    await page.reload();
    await visible(page.getByRole("table", { name: "Delivery log" }).getByText("An agent is waiting for your approval").first(), "the entry in the log table");
  });

  await step("l4. Settings: personal notification preferences with quiet hours save and survive a reload", async () => {
    await page.goto(`${base}/#/profile`);
    await page.reload();
    const table = page.getByRole("table", { name: "My notifications" });
    await visible(table, "my notifications table");
    await page.getByLabel("Quiet hours", { exact: true }).click();
    await page.getByLabel("Hourly digest").click();
    await page.getByRole("button", { name: "Save notification settings" }).click();
    const me = await poll(async () => {
      const v = await api("/me/notifications");
      return v.quietStart ? v : null;
    }, "preferences saved");
    eq([me.quietStart, me.quietEnd, me.digest], ["22:00", "07:00", true], "stored preferences");
    await page.reload();
    await visible(page.getByLabel("Quiet hours start"), "the quiet hours after a reload");
    // nothing outside the app is set up: the test says so instead of failing
    await page.getByRole("button", { name: /Send me a test/ }).click();
    await visible(page.getByText(/nothing to test/), "the explanation");
  });
  // ---- i: onboarding. A fresh browser context is a new person: nothing is marked as seen. -----------------------------------------
  const fresh = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  fresh.on("page", watch);
  const np = await fresh.newPage();
  shotPage = np;
  const nvisible = (loc, what) => visible(loc, what);
  let emptyId = "";
  const ishot = async (pg, name) => {
    if (!process.env.E2E_SHOTS) return;
    fs.mkdirSync(process.env.E2E_SHOTS, { recursive: true });
    await pg.waitForTimeout(500);
    await pg.screenshot({ path: path.join(process.env.E2E_SHOTS, `${name}.png`) });
  };
  const userMenu = async (name) => {
    await np.getByRole("button", { name: "User menu" }).first().click();
    await np.getByRole("menuitem", { name }).click();
  };

  await step("i1. first run: the welcome appears for a new person, can be skipped, stays away and is reopened from the user menu", async () => {
    await np.goto(`${base}/#/`);
    const dlg = np.getByRole("dialog", { name: "Welcome to 7ar" });
    await nvisible(dlg, "the welcome dialog");
    await nvisible(dlg.getByRole("heading", { name: "Project" }), "the first idea");
    await ishot(np, "onboarding-welcome");
    await dlg.getByRole("button", { name: "Next" }).click();
    await nvisible(dlg.getByRole("heading", { name: "Session" }), "the second idea");
    await dlg.getByRole("button", { name: "Skip for now" }).click();
    await gone(np.getByRole("dialog"), "the welcome closed");
    await np.reload();
    await nvisible(np.getByRole("main").getByRole("heading", { name: "Dashboard" }), "the dashboard");
    eq(await np.getByRole("dialog", { name: "Welcome to 7ar" }).count(), 0, "the welcome stays away once seen");
    await userMenu("Getting started");
    await nvisible(np.getByRole("dialog", { name: "Welcome to 7ar" }), "the welcome reopened from the user menu");
    for (let i = 0; i < 4; i++) await np.getByRole("dialog").getByRole("button", { name: /^(Next|Where to start)$/ }).click();
    await nvisible(np.getByRole("dialog").getByRole("button", { name: /Try it with a sample/ }), "the first actions");
    await ishot(np, "onboarding-first-actions");
    await np.keyboard.press("Escape");
    await gone(np.getByRole("dialog"), "the welcome closed with Escape");
  });

  await step("i2. the getting-started checklist shows real progress and can be dismissed", async () => {
    await np.goto(`${base}/#/`);
    const main = np.getByRole("main");
    const card = main.getByRole("region", { name: "Getting started" });
    await nvisible(card, "the checklist on the dashboard");
    // the server has projects and sessions by now: those steps are ticked from the data, the teammate step is a link to Members
    const bar = card.getByRole("progressbar", { name: "Getting started progress" });
    const done = await card.getByText(/\(done\)/).count();
    eq(await bar.getAttribute("aria-valuenow"), String(done), "the progress bar counts the ticked steps");
    if (done < 2) throw new Error(`expected the project and session steps to be ticked, got ${done}`);
    await nvisible(card.getByRole("button", { name: /Create a project.*\(done\)/ }), "the project step ticked");
    await nvisible(card.getByRole("button", { name: /Start your first session.*\(done\)/ }), "the session step ticked");
    const member = card.getByRole("link", { name: /Invite a teammate/ });
    if (!/tab=members/.test((await member.getAttribute("href")) ?? "")) throw new Error("the teammate step does not link to the members tab");
    const explorer = np.getByRole("navigation", { name: "Explorer" }).getByRole("region", { name: "Getting started" });
    await nvisible(explorer, "the explorer card");
    await ishot(np, "onboarding-dashboard-checklist");
    await card.getByRole("button", { name: "Dismiss" }).click();
    await gone(card, "the checklist dismissed");
    eq(await explorer.filter({ visible: true }).count(), 0, "the explorer card is dismissed with it");
    await np.reload();
    await nvisible(main.getByRole("heading", { name: "Dashboard" }), "the dashboard again");
    eq(await main.getByRole("region", { name: "Getting started" }).count(), 0, "dismissed for good");
  });

  await step("i3. empty states teach: what it is, the first action and a Learn more link to the help", async () => {
    const empty = (emptyId = (await post("/projects", { name: "E2E empty guides" })).id);
    await np.goto(`${base}/#/p/${empty}?tab=board`);
    const main = np.getByRole("main");
    const guide = main.getByRole("region", { name: "No sessions yet" });
    await nvisible(guide, "the board guide");
    await nvisible(guide.getByRole("button", { name: "Start your first session" }), "its primary action");
    await ishot(np, "onboarding-empty-board");
    for (const [tab, title, action] of [
      ["requirements", "No requirements yet", "Write your first requirement"],
      ["tasks", "No tasks yet", "Create your first task"],
      ["schedules", "No schedules yet", "Create your first schedule"],
      ["workflows", "No workflows yet", "Start with a blank workflow"],
    ]) {
      await np.goto(`${base}/#/p/${empty}?tab=${tab}`);
      const g = np.getByRole("main").getByRole("region", { name: title });
      await nvisible(g, `the ${tab} guide`);
      await nvisible(g.getByRole("button", { name: action }), `${tab}: ${action}`);
      await nvisible(g.getByRole("link", { name: "Learn more" }), `${tab}: Learn more`);
    }
    await np.goto(`${base}/#/p/${empty}?tab=tasks`);
    await np.getByRole("main").getByRole("region", { name: "No tasks yet" }).getByRole("link", { name: "Learn more" }).click();
    await nvisible(np.getByRole("main").getByRole("heading", { name: "Hand off a task and review the pull request", level: 2 }), "the matching help article");
  });

  await step("i4. the help center: search finds an article, deep links open it", async () => {
    await np.goto(`${base}/#/help`);
    const main = np.getByRole("main");
    await main.getByRole("searchbox", { name: "Search the help" }).fill("autonomous");
    const hit = main.getByRole("list", { name: "Search results" }).getByRole("button", { name: /Policies/ });
    await nvisible(hit, "the policies article in the results");
    await hit.click();
    await nvisible(main.getByRole("heading", { name: /^Policies/, level: 2 }), "the article");
    await nvisible(main.getByRole("cell", { name: /Supervised/ }), "the policy table");
    await ishot(np, "onboarding-help-article");
    await np.goto(`${base}/#/help?a=budgets`);
    await nvisible(np.getByRole("main").getByRole("heading", { name: "Budgets and spend caps", level: 2 }), "a deep link opens its article");
    await nvisible(np.getByRole("tab", { name: "Help" }), "Help is a tab");
  });

  await step("i5. contextual help: a ? beside a section opens its article in a side sheet", async () => {
    const empty = emptyId;
    await np.goto(`${base}/#/p/${empty}?tab=tasks`);
    await np.getByRole("main").getByRole("button", { name: "Tasks (help)" }).first().click();
    const sheet = np.getByRole("dialog");
    await nvisible(sheet.getByRole("heading", { name: "Tasks" }), "the article in the sheet");
    await np.keyboard.press("Escape");
    await gone(np.getByRole("dialog"), "the sheet closed");
    // the policy of a new task has its own ? and says in one line what it means
    await np.getByRole("main").getByRole("button", { name: "Create your first task" }).click();
    const dlg = np.getByRole("dialog");
    await dlg.getByRole("button", { name: "Policy (help)" }).click();
    await nvisible(np.getByRole("dialog", { name: /Policies/ }), "the policies article over the form");
    await np.keyboard.press("Escape");
    // model, budget and attempts are behind Advanced
    await nvisible(dlg.getByRole("button", { name: /Advanced options/ }), "advanced options");
    eq(await dlg.getByLabel("Budget (USD)").count(), 0, "the budget is hidden until Advanced is open");
    await dlg.getByRole("button", { name: /Advanced options/ }).click();
    await nvisible(dlg.getByLabel("Budget (USD)"), "the budget under Advanced");
    await np.keyboard.press("Escape");
  });

  await step("i6. the guided tour runs from the menu, moves with the keyboard and closes; ? and the palette teach the shortcuts", async () => {
    await np.goto(`${base}/#/`);
    await userMenu("Take the tour");
    const tour = np.getByTestId("tour");
    await nvisible(tour.getByRole("heading", { name: "The explorer" }), "the first step");
    await ishot(np, "onboarding-tour");
    await np.keyboard.press("ArrowRight");
    await nvisible(tour.getByRole("heading", { name: "Editor tabs" }), "the tabs step");
    await tour.getByRole("button", { name: "Next" }).click();
    await nvisible(tour.getByRole("heading", { name: "Needs you" }), "the Needs you step");
    await tour.getByRole("button", { name: "Next" }).click();
    await nvisible(tour.getByRole("heading", { name: "The status bar" }), "the status bar step");
    await tour.getByRole("button", { name: "Next" }).click();
    await nvisible(tour.getByRole("heading", { name: /Ctrl\/Cmd \+ K/ }), "the palette step");
    await tour.getByRole("button", { name: "Done" }).click();
    await gone(tour, "the tour closed");
    await userMenu("Take the tour");
    await nvisible(tour, "the tour replayed");
    await np.keyboard.press("Escape");
    await gone(tour, "the tour closed with Escape");
    await np.keyboard.press("?");
    await nvisible(np.getByRole("dialog", { name: "Keyboard shortcuts" }), "the shortcuts sheet");
    await np.keyboard.press("Escape");
    await gone(np.getByRole("dialog"), "the shortcuts closed");
    await np.keyboard.press("Control+k");
    await nvisible(np.getByRole("option", { name: /Try: start a session/ }), "a suggestion in the palette");
    await nvisible(np.getByRole("option", { name: "Open help" }), "the help command");
    await np.keyboard.press("Escape");
  });

  await step("i7. try it with a sample: a project from a template and a first session that starts working", async () => {
    await np.goto(`${base}/#/`);
    await userMenu("Getting started");
    await nvisible(np.getByRole("dialog", { name: "Welcome to 7ar" }), "the welcome");
    const dlg = np.getByRole("dialog"); // its title changes on the last step
    for (let i = 0; i < 4; i++) await dlg.getByRole("button", { name: /^(Next|Where to start)$/ }).click();
    await dlg.getByRole("button", { name: /Try it with a sample/ }).click();
    await np.waitForURL(/#\/t\/[^/]+$/);
    const id = np.url().split("/t/")[1];
    const t = await poll(async () => {
      const x = await thread(id);
      return x.messages.length ? x : null;
    }, "the sample's first message");
    if (!t.projectId) throw new Error("the sample session is not in a project");
    await idle(id);
  });

  await fresh.close();

  // ---- m: the weak spots closed (stream M) ------------------------------------------------------------------------
  // A published workflow made through the API (the designer is covered by h1); nodes get a position.
  const mkWorkflow = async (pid, name, nodes, inputs = []) => {
    const w = await post(`/projects/${pid}/workflows`, { name, inputs, nodes: nodes.map((n, i) => ({ pos: { x: i * 220, y: 0 }, ...n })) });
    return post(`/projects/${pid}/workflows/${w.id}/publish`, {});
  };
  const startRun = async (pid, workflowId, inputs = {}) => {
    const r = await post(`/projects/${pid}/workflow-runs`, { workflowId, inputs });
    return (r.run ?? r).id;
  };
  const runOf = async (pid, id) => (await api(`/projects/${pid}/workflow-runs/${id}`)).run;
  const stepOf = (r, id) => r.steps.find((s) => s.nodeId === id);
  const openRun = async (pid, id) => {
    await page.goto(`${base}/#/p/${pid}?tab=workflows&run=${id}`);
    await page.reload();
    await visible(page.getByRole("group", { name: "Workflow canvas" }), "the run view");
  };

  await step("m1. Workflow run controls: pause, resume, add context, send back, approve, rerun from a step, take over, cancel, rerun a cancelled run", async () => {
    const hp = (await post("/projects", { name: "E2E run controls" })).id;
    const wf = await mkWorkflow(hp, "Controls", [
      { id: "plan", kind: "agent", title: "Plan it", prompt: "Plan it." },
      { id: "review", kind: "gate", title: "Review gate", approvers: ["role:member"], dependsOn: ["plan"] },
      { id: "build", kind: "agent", title: "Build it", prompt: "Build it.", dependsOn: ["review"] },
      { id: "signoff", kind: "gate", title: "Sign-off", approvers: ["role:member"], dependsOn: ["build"] },
    ]);
    const runId = await startRun(hp, wf.id);
    await openRun(hp, runId);
    const canvas = page.getByRole("group", { name: "Workflow canvas" });
    await visible(canvas.locator('[data-kind="gate"][data-state="waiting"]'), "the review gate waits", 60_000);
    // pause and resume
    await page.getByRole("button", { name: "Pause", exact: true }).click();
    await visible(page.getByText(/Paused: steps that are running finish/), "the paused notice");
    eq((await runOf(hp, runId)).state, "paused", "stored paused");
    await page.getByRole("button", { name: "Resume", exact: true }).click();
    await gone(page.getByText(/Paused: steps that are running finish/), "the paused notice is gone");
    eq((await runOf(hp, runId)).state, "running", "stored running");
    // add context to the gate: the gate keeps waiting
    const gate = page.getByRole("region", { name: "Step Review gate" });
    await gate.getByRole("button", { name: "Add context" }).click();
    await gate.getByLabel("A note the next steps are given").fill("Use the staging database.");
    await gate.getByRole("button", { name: "Add the note" }).click();
    await poll(async () => stepOf(await runOf(hp, runId), "review").context?.length === 1, "the note is stored on the gate");
    eq(stepOf(await runOf(hp, runId), "review").state, "waiting", "the gate keeps waiting");
    // send back: the step before runs again with the feedback, then the gate waits again (round 2)
    await gate.getByRole("button", { name: "Send back", exact: true }).click();
    await gate.getByLabel(/What must change/).fill("Tighten the scope.");
    await gate.getByRole("button", { name: "Send it back" }).click();
    await poll(async () => {
      const r = await runOf(hp, runId);
      return stepOf(r, "plan").round === 2 && /Tighten the scope/.test(stepOf(r, "plan").feedback ?? "") && stepOf(r, "review").round === 2 && stepOf(r, "review").state === "waiting";
    }, "the plan ran again with the feedback and the gate waits in its second round", 60_000);
    // approve: the agent step runs (the fake model answers) and the sign-off gate waits
    await gate.getByRole("button", { name: "Approve", exact: true }).click();
    await poll(async () => {
      const r = await runOf(hp, runId);
      return stepOf(r, "build").state === "done" && stepOf(r, "signoff").state === "waiting";
    }, "build done, sign-off waiting", 60_000);
    // rerun from the agent step: it runs again (round 2) and the sign-off waits again
    await canvas.getByRole("button", { name: "Step Build it" }).click();
    const build = page.getByRole("region", { name: "Step Build it" });
    await build.getByRole("button", { name: "Rerun from here" }).click();
    await page.getByRole("alertdialog").getByLabel(/What should this attempt do differently/).fill("Shorter, please.");
    await page.getByRole("alertdialog").getByRole("button", { name: "Rerun", exact: true }).click();
    await poll(async () => {
      const r = await runOf(hp, runId);
      return stepOf(r, "build").round === 2 && stepOf(r, "build").state === "done" && stepOf(r, "signoff").state === "waiting";
    }, "build ran again and the sign-off waits again", 60_000);
    // take over the sign-off: a person gives its result and the run is done
    await canvas.getByRole("button", { name: "Step Sign-off" }).click();
    const sign = page.getByRole("region", { name: "Step Sign-off" });
    await sign.getByRole("button", { name: "Take over" }).click();
    await page.getByRole("alertdialog").getByLabel("The result").fill("Signed off by hand.");
    await page.getByRole("alertdialog").getByRole("button", { name: "Mark as done" }).click();
    await poll(async () => (await runOf(hp, runId)).state === "done", "the run is done after the take-over");
    const s = stepOf(await runOf(hp, runId), "signoff");
    eq([s.manual, s.result], [true, "Signed off by hand."], "the taken-over step");
    await visible(page.getByText("done by hand"), "the step says it was done by hand");

    // a second run: cancel it, then rerun from the first step (it works on a cancelled run)
    const second = await startRun(hp, wf.id);
    await openRun(hp, second);
    await visible(page.getByRole("group", { name: "Workflow canvas" }).locator('[data-kind="gate"][data-state="waiting"]'), "the gate of the second run", 60_000);
    await page.getByRole("button", { name: "Cancel run", exact: true }).click();
    await page.getByRole("alertdialog").getByRole("button", { name: "Cancel the run" }).click();
    await poll(async () => (await runOf(hp, second)).state === "cancelled", "the run is cancelled");
    await page.getByRole("group", { name: "Workflow canvas" }).getByRole("button", { name: "Step Review gate" }).click();
    await page.getByRole("region", { name: "Step Review gate" }).getByRole("button", { name: "Rerun from here" }).click();
    await page.getByRole("alertdialog").getByRole("button", { name: "Rerun", exact: true }).click();
    await poll(async () => {
      const r = await runOf(hp, second);
      return r.state === "running" && stepOf(r, "review").state === "waiting";
    }, "the cancelled run runs again from the gate");
  });

  await step("m2. A subworkflow step runs a child run and finishes with it", async () => {
    const hp = (await post("/projects", { name: "E2E subworkflow" })).id;
    const child = await mkWorkflow(hp, "Child", [{ id: "c1", kind: "agent", title: "Child step", prompt: "Say hello." }]);
    const parent = await mkWorkflow(hp, "Parent", [{ id: "sub", kind: "subworkflow", title: "Run the child", workflowId: child.id }]);
    const runId = await startRun(hp, parent.id);
    await poll(async () => (await runOf(hp, runId)).state === "done", "the parent run is done", 60_000);
    const runs = await api(`/projects/${hp}/workflow-runs`);
    eq(runs.length, 2, "a parent and a child run");
    const childRun = runs.find((r) => r.parentRunId === runId);
    if (!childRun || childRun.workflowName !== "Child" || childRun.state !== "done") throw new Error(`the child run: ${JSON.stringify(childRun)}`);
    await openRun(hp, runId);
    await page.getByRole("group", { name: "Workflow canvas" }).getByRole("button", { name: "Step Run the child" }).click();
    await visible(page.getByRole("region", { name: "Step Run the child" }).getByText(/Runs the workflow as run/), "the step names its child run");
    // the child is a run of its own, listed with the others
    await page.goto(`${base}/#/p/${hp}?tab=workflows`);
    await page.reload();
    await visible(page.getByText("Child", { exact: true }).first(), "the child workflow is listed");
  });

  await step("m3. Schedules: edit, 'keep one conversation' reuses the session, delete", async () => {
    const hp = (await post("/projects", { name: "E2E schedule edit" })).id;
    const created = await post(`/projects/${hp}/schedules`, { name: "Morning hello", prompt: "Say hello to the team.", cron: "@daily", timezone: "UTC", enabled: true });
    await page.goto(`${base}/#/p/${hp}?tab=schedules`);
    await page.reload();
    const list = page.getByRole("list", { name: "Schedules" });
    await list.getByRole("button", { name: "Edit Morning hello" }).click();
    const dlg = page.getByRole("dialog");
    await dlg.getByLabel("Name").fill("Evening hello");
    await dlg.getByLabel("Keep one conversation").click();
    await dlg.getByRole("button", { name: "Save", exact: true }).click();
    await gone(page.getByRole("dialog"), "the schedule form closed");
    await visible(list.getByText("Evening hello"), "the renamed schedule");
    const stored = (await api(`/projects/${hp}/schedules`))[0];
    eq([stored.id, stored.name, stored.reuseSession], [created.id, "Evening hello", true], "stored edit");
    // two manual runs land in one conversation
    const runsOf = () => api(`/projects/${hp}/schedules/${created.id}/runs?limit=10`);
    await list.getByRole("button", { name: "Run Evening hello now" }).click();
    await poll(async () => (await runsOf()).filter((r) => r.status === "succeeded").length === 1, "the first run succeeded", 60_000);
    await list.getByRole("button", { name: "Run Evening hello now" }).click();
    await poll(async () => (await runsOf()).filter((r) => r.status === "succeeded").length === 2, "the second run succeeded", 60_000);
    const threads = new Set((await runsOf()).map((r) => r.threadId));
    eq(threads.size, 1, "both runs used one conversation");
    const th = await thread([...threads][0]);
    eq(th.messages.filter((m) => m.role === "user").length, 2, "the conversation holds both prompts");
    // delete it
    await list.getByRole("button", { name: "Delete Evening hello" }).click();
    await page.getByRole("alertdialog").getByRole("button", { name: "Delete", exact: true }).click();
    await gone(list.getByText("Evening hello"), "the schedule is gone from the list");
    // the list hides the row as soon as the delete is confirmed; the store follows a moment later (slower on Postgres)
    await poll(async () => (await api(`/projects/${hp}/schedules`)).length === 0, "stored: no schedule left");
  });

  await step("m4. Steer now: a queued message goes into the reply that is running", async () => {
    const id = await newSession("E2E steer now");
    await send("slow reply please");
    await fake.held();
    await visible(page.getByText(/Slow reply finally/), "the partial reply while the run is held");
    await composer.fill("queued alpha message");
    await page.getByRole("button", { name: "Queue", exact: true }).click();
    const queue = page.getByRole("region", { name: "Queued messages" });
    await visible(queue.getByText("1 message queued"), "one queued message");
    await queue.getByRole("button", { name: "Steer now: queued alpha message" }).click();
    await poll(async () => (await api(`/threads/${id}/queue`)).length === 0, "the queue is empty after steering");
    await gone(queue, "the queue card is gone");
    fake.release();
    await visible(page.getByText("Slow reply finally done."), "the held reply completes");
    // the steered message was taken into the conversation (as a steer, or sent as the next message when the reply had ended)
    await poll(async () => (await thread(id)).messages.some((m) => m.role === "user" && m.content.includes("queued alpha message")), "the message is in the conversation", 30_000);
    await idle(id);
    eq(await page.getByText("Beta answer.").count(), 0, "nothing else was sent");
  });

  await step("m5. A second environment: the choice is offered, frozen once the session has a workspace, and a new session opens in the other one", async () => {
    // A second server with two sandbox profiles: the shared one has a single environment (h4 asserts the selector is hidden).
    const d = path.join(tmp, "second");
    for (const x of ["home", "workspace", "state"]) fs.mkdirSync(path.join(d, x), { recursive: true });
    const env2 = { ...env, SANDBOX_IMAGES: "work=img-work,coding=img-coding", HOME: path.join(d, "home"), LOCAL_WORKSPACE: path.join(d, "workspace"), LOCAL_STATE_DIR: path.join(d, "state"), HARNESS_DOCUMENTS_DIR: path.join(d, "documents"), HARNESS_JOBS_DIR: path.join(d, "jobs") };
    const child2 = spawn(bin, ["-local", "-team"], { cwd: d, env: env2, stdio: ["ignore", "pipe", "pipe"] });
    extraServers.push(child2);
    const base2 = await new Promise((resolve, reject) => {
      let out = "";
      const timer = setTimeout(() => reject(new Error(`the second harness-web did not start:\n${out}`)), 30_000);
      child2.stdout.on("data", (x) => {
        out += x;
        const m = /harness-web: (http:\/\/[^/\s]+)\//.exec(out);
        if (m) (clearTimeout(timer), resolve(m[1]));
      });
      child2.once("exit", (code) => (clearTimeout(timer), reject(new Error(`the second harness-web exited (${code}):\n${out}`))));
    });
    const api2 = async (p, init) => {
      const r = await fetch(`${base2}/api${p}`, init);
      if (!r.ok) throw new Error(`${init?.method ?? "GET"} ${base2}/api${p} -> ${r.status}`);
      return r.json();
    };
    const post2 = (p, body, method = "POST") => api2(p, { method, headers: { "Content-Type": "application/json" }, body: JSON.stringify(body ?? {}) });
    const envs = await api2("/environments");
    eq(envs.environments.map((e) => e.name).sort(), ["coding", "work"], "two environments are offered");
    const th = await post2("/threads", { title: "E2E environments" });
    await page.goto(`${base2}/#/t/${th.id}`);
    await visible(composer, "composer");
    const select = page.getByRole("combobox", { name: "Runtime environment" });
    await visible(select, "the environment selector");
    eq(envs.default, "coding", "the local backend's default environment");
    await select.selectOption("work"); // not the default: the choice is recorded
    await poll(async () => (await api2(`/threads/${th.id}/environment`)).environment === "work", "the choice is stored");
    // the first run creates the workspace: the choice is frozen
    await send("please write a file");
    await fake.held();
    fake.release();
    await poll(async () => (await api2(`/threads/${th.id}/environment`)).frozen === true, "the environment is frozen after the first run", 30_000);
    await page.reload();
    await visible(page.getByText(/Fixed once the session has a workspace/), "the selector says it is fixed");
    await page.getByRole("combobox", { name: "Runtime environment" }).selectOption("coding");
    await visible(page.getByText(/This session already has a workspace/), "another choice offers a new session");
    await page.getByRole("button", { name: "Open a new session in coding" }).click();
    await poll(() => /#\/t\/[^/]+$/.test(page.url()) && !page.url().endsWith(th.id), "a new session opens");
    const next = page.url().split("/t/")[1];
    await poll(async () => (await api2(`/threads/${next}/environment`)).environment === "coding", "the new session is in the other environment");
    await page.goto(`${base}/#/`); // back to the main server for the next scenarios
  });

  await step("m6. Over the cap: a factory task and a workflow run are refused with the reason", async () => {
    const hp = (await post("/projects", { name: "E2E over the cap" })).id;
    // some spend first (one scripted call), then a cap that is already used up
    const th = await post("/threads", { title: "spend", projectId: hp });
    await post(`/threads/${th.id}/runs`, { message: "hello", model: "gpt-test" });
    await idle(th.id);
    await post(`/projects/${hp}/usage/cap`, { monthlyUsd: 0.0001 }, "PUT");
    // a factory task
    await post(`/projects/${hp}/tasks`, { title: "Over the cap", spec: "Say hello." });
    const task = await poll(async () => {
      const t = (await api(`/projects/${hp}/tasks?limit=20`)).find((x) => x.title === "Over the cap");
      return t && ["blocked", "failed"].includes(t.state) ? t : null;
    }, "the task is refused", 60_000);
    if (!/cap/i.test(`${task.blockedReason ?? ""} ${task.error ?? ""}`)) throw new Error(`the task does not say why: ${JSON.stringify([task.state, task.blockedReason, task.error])}`);
    await page.goto(`${base}/#/p/${hp}?tab=tasks&task=${task.id}`);
    await page.reload();
    await visible(page.getByRole("region", { name: "Task detail" }).getByText(/cap/i).first(), "the reason is on screen in the task detail");
    // a workflow run with an agent step
    const wf = await mkWorkflow(hp, "Capped", [{ id: "a", kind: "agent", title: "Say hello", prompt: "Say hello." }]);
    const runId = await startRun(hp, wf.id);
    // the step's task is refused three times and then blocked: the step says why, and the run waits for a person
    const run = await poll(async () => {
      const r = await runOf(hp, runId);
      const st = stepOf(r, "a");
      return st.state === "failed" || /cap/i.test(st.note ?? "") ? r : null;
    }, "the step reports the refusal", 60_000);
    const st = stepOf(run, "a");
    if (!/cap/i.test(`${st.note ?? ""} ${st.error ?? ""}`)) throw new Error(`the step does not say why: ${JSON.stringify(st)}`);
    eq(st.taskState === "blocked" || st.state === "failed", true, "the step's task is blocked (or the step failed)");
    await openRun(hp, runId);
    await visible(page.getByRole("region", { name: "Step Say hello" }).getByText(/cap/i).first(), "the reason is on screen in the step");
    await post(`/workflow-runs/${runId}/cancel`, {});
    await post(`/projects/${hp}/usage/cap`, { monthlyUsd: 0 }, "PUT");
  });

  await step("m7. Workflow script steps run in the run's sandbox without a model; a supervised one asks first", async () => {
    const hp = (await post("/projects", { name: "E2E script steps" })).id;
    const wf = await mkWorkflow(hp, "Scripts", [
      { id: "hello", kind: "script", title: "Echo", command: "echo script-says-hi && echo to-stderr 1>&2" },
      { id: "risky", kind: "script", title: "Supervised echo", command: "echo approved-and-ran > ran-by-workflow.txt && cat ran-by-workflow.txt", policy: "supervised", dependsOn: ["hello"] },
      { id: "bad", kind: "script", title: "Failing", command: "echo about-to-fail; exit 3", dependsOn: ["risky"], onFailure: { action: "continue" } },
    ]);
    const requestsBefore = fake.requests.length;
    const runId = await startRun(hp, wf.id);
    await poll(async () => stepOf(await runOf(hp, runId), "risky").state === "waiting", "the supervised script waits for approval", 60_000);
    const first = stepOf(await runOf(hp, runId), "hello");
    if (first.state !== "done" || first.taskId || !first.direct || !first.log.includes("script-says-hi")) throw new Error(`the first script: ${JSON.stringify(first)}`);
    eq(fake.requests.length, requestsBefore, "no model call was made for the script steps");
    await openRun(hp, runId);
    const canvas = page.getByRole("group", { name: "Workflow canvas" });
    await canvas.getByRole("button", { name: "Step Echo" }).click();
    await visible(page.getByRole("region", { name: "Step Echo" }).getByLabel("Step output").getByText(/script-says-hi/), "the output of the script in the step");
    await visible(page.getByRole("region", { name: "Step Echo" }).getByText(/Ran in the run's sandbox, no model involved \(exit code 0\)/), "the step says no model was involved");
    await canvas.getByRole("button", { name: "Step Supervised echo" }).click();
    const risky = page.getByRole("region", { name: "Step Supervised echo" });
    await visible(risky.getByText(/waits for your approval before it runs/), "the approval is asked for");
    await risky.getByRole("button", { name: "Approve", exact: true }).click();
    await poll(async () => {
      const r = await runOf(hp, runId);
      return stepOf(r, "risky").state === "done" && stepOf(r, "bad").state === "failed" && r.state === "done";
    }, "the approved script ran, the failing one failed and the run went on", 60_000);
    const bad = stepOf(await runOf(hp, runId), "bad");
    eq(bad.exitCode, 3, "the exit code of the failing script");
    if (!bad.log.includes("about-to-fail") || !/status 3/.test(bad.error)) throw new Error(`the failing step: ${JSON.stringify(bad)}`);
    eq(fake.requests.length, requestsBefore, "still no model call");
  });

  await step("m8. Queued messages carry an image; Discard all empties the queue", async () => {
    const id = await newSession("E2E queue image");
    await send("slow reply please");
    await fake.held();
    const png = Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==", "base64");
    // the attach button opens a file chooser; the file is picked there
    const attach = async (name) => {
      const [chooser] = await Promise.all([page.waitForEvent("filechooser"), page.getByRole("button", { name: "Attach an image" }).click()]);
      await chooser.setFiles({ name, mimeType: "image/png", buffer: png });
      await visible(page.getByText(name, { exact: true }).first(), `the attached ${name} in the composer`);
    };
    await attach("shot.png");
    await composer.fill("queued alpha with an image");
    await page.getByRole("button", { name: "Queue", exact: true }).click();
    const queue = page.getByRole("region", { name: "Queued messages" });
    await visible(queue.getByText("shot.png"), "the image on the queued item");
    eq((await api(`/threads/${id}/queue`))[0].attachments.map((a) => a.name), ["shot.png"], "the server queue keeps the attachment");
    await composer.fill("queued beta message");
    await page.getByRole("button", { name: "Queue", exact: true }).click();
    await visible(queue.getByText("2 messages queued"), "two queued messages");
    await queue.getByRole("button", { name: /Discard all/ }).click();
    await poll(async () => (await api(`/threads/${id}/queue`)).length === 0, "the queue is empty after Discard all");
    await gone(queue, "the queue card is gone");
    // an image queued again travels with the message when it is sent
    await attach("again.png");
    await composer.fill("queued alpha again");
    await page.getByRole("button", { name: "Queue", exact: true }).click();
    await visible(queue.getByText("again.png"), "the second queued image");
    fake.release();
    await visible(page.getByText("Slow reply finally done."), "the held reply completes");
    await visible(page.getByText("Alpha answer."), "the queued message was sent and answered");
    await idle(id);
    const sent = (await thread(id)).messages.find((m) => m.role === "user" && m.content === "queued alpha again");
    eq(sent?.metadata?.attachments?.map((a) => a.name), ["again.png"], "the sent message has the image");
    eq(await page.getByText("Beta answer.").count(), 0, "the discarded message was never sent");
  });

  await step("m9. Personal context, My roles, the role chip and the executors' availability", async () => {
    // a personal session: the explorer says Personal and shows no project's work
    const th = await post("/threads", { title: "E2E personal" });
    await page.goto(`${base}/#/t/${th.id}`);
    await page.reload();
    await visible(composer, "composer");
    const explorer = page.getByRole("navigation", { name: "Explorer" });
    await visible(explorer.getByText(/Personal\. Pick a project/), "the explorer is in the Personal context");
    eq(await explorer.getByRole("region", { name: "Work" }).count(), 0, "no project work in the personal context");
    eq(await page.getByTestId("agent-role").count(), 0, "no role chip before a role is used");
    // My roles: same editor, caller's own roles
    await page.goto(`${base}/#/roles`);
    await page.reload();
    await visible(page.getByRole("heading", { name: "My roles" }), "the My roles page");
    await page.getByRole("button", { name: /New role/ }).click();
    const dlg = page.getByRole("dialog");
    await dlg.getByLabel("Name").fill("my-reviewer");
    await dlg.getByLabel("Instructions").fill("Review kindly.");
    await dlg.getByRole("button", { name: "Create" }).click();
    await gone(page.getByRole("dialog"), "the role editor closed");
    await visible(page.getByRole("list", { name: "Roles" }).getByText("my-reviewer"), "the personal role in the list");
    eq((await api("/me/roles")).some((r) => r.name === "my-reviewer" && r.scope === "user"), true, "stored personal role");
    // a run in the role: the session header shows it
    await post(`/threads/${th.id}/runs`, { message: "hello", model: "gpt-test", role: "my-reviewer" });
    await idle(th.id);
    await page.goto(`${base}/#/t/${th.id}`);
    await page.reload();
    await visible(page.getByTestId("agent-role").filter({ hasText: "my-reviewer" }), "the role chip shows the role of the last run");
    // executors: the availability badge and the keys each one reads
    await page.goto(`${base}/#/profile`);
    await page.reload();
    const card = page.getByRole("list", { name: "Executors" });
    const first = card.getByRole("listitem").first();
    await visible(first.getByTestId("executor-availability"), "the availability badge", 15_000);
    const label = (await first.getByTestId("executor-availability").innerText()).trim();
    if (!["ready", "will be installed on first use", "not in the sandbox image", "unknown"].includes(label)) throw new Error(`availability badge: ${label}`);
    await visible(first.getByText(/Reads .*_API_KEY/), "the keys the executor reads");
  });

  // Opt-in: E2E_SHOTS=<dir> saves screenshots of the main surfaces (not a scenario, nothing is asserted).
  if (process.env.E2E_SHOTS) {
    const dir = process.env.E2E_SHOTS;
    fs.mkdirSync(dir, { recursive: true });
    const shot = async (pg, url, name) => {
      await pg.goto(`${base}/#${url}`);
      await pg.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));
      await pg.waitForTimeout(1500);
      await pg.screenshot({ path: path.join(dir, `${name}.png`) });
    };
    const runs = await api(`/projects/${proj.wf}/workflow-runs`);
    await shot(page, "/", "dashboard");
    // a session with an edit, so the Diff tab shows
    await page.goto(`${base}/#/t/${sessionState.w}`);
    await page.getByRole("textbox", { name: "Message" }).fill("please edit the report");
    await page.getByRole("button", { name: "Send", exact: true }).click();
    await visible(page.getByText("Edited the report.", { exact: true }), "the edit answer");
    await idle(sessionState.w);
    await shot(page, `/t/${sessionState.w}`, "session");
    const reqs = await api(`/projects/${proj.id}/requirements`);
    await shot(page, `/p/${proj.id}?tab=requirements&req=${reqs[0]?.id ?? ""}`, "requirement");
    const tasks = await api(`/projects/${proj.id}/tasks?limit=50`);
    await shot(page, `/p/${proj.id}?tab=tasks&task=${tasks[0]?.id ?? ""}`, "task");
    await shot(page, `/p/${proj.id}?tab=board`, "board");
    await shot(page, `/p/${proj.wf}?tab=workflows&run=${runs[0]?.id ?? ""}`, "workflow-run");
    await shot(page, `/p/${proj.id}?tab=settings`, "settings");
    
    for (const t of ["requirements", "roles", "members", "memory", "capabilities", "usage", "insights", "schedules", "audit", "recipes"]) await shot(page, `/p/${proj.id}?tab=${t}`, `tab-${t}`);
    for (const r of ["/insights", "/inbox", "/profile", "/jobs", "/admin", "/connectors"]) await shot(page, r, `page${r.replace("/", "-")}`);
    await shot(page, `/p/${proj.wf}?tab=workflows`, "workflows");
    await shot(page, `/p/${lp.id}?tab=notifications`, "notifications-project");
    await shot(page, "/profile", "notifications-me");
    await page.getByRole("table", { name: "My notifications" }).scrollIntoViewIfNeeded();
    await page.getByLabel("Quiet hours", { exact: true }).scrollIntoViewIfNeeded();
    await page.screenshot({ path: path.join(dir, "notifications-me.png") });
    await page.evaluate(() => localStorage.setItem("theme", "light"));
    await page.reload();
    await shot(page, `/p/${proj.id}?tab=board`, "board-light");
    await shot(page, `/t/${sessionState.w}`, "session-light");
    await shot(page, `/p/${proj.id}?tab=insights`, "insights-light");
    await page.evaluate(() => localStorage.setItem("theme", "dark"));
    const phone = await browser.newContext({ viewport: { width: 390, height: 844 } });
    await phone.addInitScript(onboarded);
    const pp = await phone.newPage();
    await shot(pp, `/t/${sessionState.w}`, "phone-session");
    await shot(pp, `/p/${proj.id}?tab=board`, "phone-board");
    await shot(pp, `/p/${proj.id}?tab=insights`, "phone-insights");
    await pp.getByRole("button", { name: "Open navigation" }).click();
    await pp.waitForTimeout(600);
    await pp.screenshot({ path: path.join(dir, "phone-explorer.png") });
    await phone.close();
    // onboarding on a phone: the dashboard with the checklist (a person who has been welcomed)
    const phone2 = await browser.newContext({ viewport: { width: 390, height: 844 } });
    await phone2.addInitScript(() => {
      try {
        localStorage.setItem("7ar.onboarding.v1.local", JSON.stringify({ welcomed: true }));
      } catch {
        /* no storage */
      }
    });
    await shot(await phone2.newPage(), "/", "onboarding-phone-dashboard");
    await phone2.close();
  }

  await ctx.close();
}

try {
  await main();
} catch (e) {
  failed = true;
  if (!results.some((r) => !r.ok)) results.push({ name: "setup", ok: false, ms: 0, err: e.stack ?? String(e) });
} finally {
  await cleanup();
}

for (const r of results) {
  console.log(`${r.ok ? "PASS" : "FAIL"}  ${r.name} (${(r.ms / 1000).toFixed(1)}s)`);
  if (!r.ok) console.log(r.err.replace(/^/gm, "      ") + (r.shot ? `\n      screenshot: ${r.shot}` : ""));
}
if (problems.length) {
  failed = true;
  console.log("browser problems:\n  " + [...new Set(problems)].join("\n  "));
}
const passed = results.filter((r) => r.ok).length;
console.log(failed ? `FAIL: ${passed}/${results.length} scenarios passed` : `PASS: ${passed}/${results.length} scenarios passed`);
process.exit(failed ? 1 : 0);
