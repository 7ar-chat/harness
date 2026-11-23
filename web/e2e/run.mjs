// Browser end-to-end test: a REAL harness-web process (built from ../app) + a scripted fake LLM (fakellm.mjs) +
// headless Chromium (playwright). No external network. Run it with `npm run e2e` from web/.
//
// Exit codes: 0 = PASS or SKIP (prerequisite missing, said on stdout), 1 = FAIL.
// Fails on any console error/warning, pageerror or CSP violation (nothing is allow-listed except what is
// documented at BENIGN below). Temp dirs and child processes are always cleaned up, also on failure and SIGINT.
import { spawn, spawnSync } from "node:child_process";
import fs from "node:fs";
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
let fake = null;
let cleanupDone = null;
const cleanup = () => (cleanupDone ??= doCleanup()); // one run, awaited by every caller (SIGINT and the normal exit)
async function doCleanup() {
  await browser?.close().catch(() => {});
  if (server && server.exitCode === null) {
    server.kill("SIGTERM");
    const gone = await Promise.race([new Promise((r) => server.once("exit", () => r(true))), new Promise((r) => setTimeout(() => r(false), 6000))]);
    if (!gone) server.kill("SIGKILL");
  }
  await fake?.close().catch(() => {});
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
    // The admin page and the git hosts need a secret key (to seal tokens and SSH keys) and an administrator.
    HARNESS_SECRET_KEY: "e2e-secret-key-0123456789abcdef0123456789",
    HARNESS_ADMINS: "local",
    // The document library and the job search are optional features whose routes answer 404 without a directory; they are on
    // so that the References and Jobs pages render their real state (h5) instead of a "not available" one.
    HARNESS_DOCUMENTS_DIR: path.join(tmp, "documents"),
    HARNESS_JOBS_DIR: path.join(tmp, "jobs"),
  };
  server = spawn(bin, ["-local", "-team"], { cwd: tmp, env, stdio: ["ignore", "pipe", "pipe"] });
  let serverLog = "";
  server.stderr.on("data", (d) => (serverLog += d));
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
      await page.screenshot({ path: shot }).then(() => (results.at(-1).shot = shot)).catch(() => {});
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
    const top = ["/", "/supervisor", "/inbox", "/jobs", "/references", "/connectors", "/capabilities", "/schedules", "/usage", "/memory", "/profile", "/admin"];
    const tabs = ["board", "overview", "members", "memory", "capabilities", "roles", "schedules", "usage", "audit", "tasks", "requirements", "workflows", "settings"];
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
    
    for (const t of ["requirements", "roles", "members", "memory", "capabilities", "usage", "schedules", "audit"]) await shot(page, `/p/${proj.id}?tab=${t}`, `tab-${t}`);
    for (const r of ["/inbox", "/profile", "/jobs", "/admin", "/connectors"]) await shot(page, r, `page${r.replace("/", "-")}`);
    await shot(page, `/p/${proj.wf}?tab=workflows`, "workflows");
    await page.evaluate(() => localStorage.setItem("theme", "light"));
    await page.reload();
    await shot(page, `/p/${proj.id}?tab=board`, "board-light");
    await shot(page, `/t/${sessionState.w}`, "session-light");
    await page.evaluate(() => localStorage.setItem("theme", "dark"));
    const phone = await browser.newContext({ viewport: { width: 390, height: 844 } });
    const pp = await phone.newPage();
    await shot(pp, `/t/${sessionState.w}`, "phone-session");
    await shot(pp, `/p/${proj.id}?tab=board`, "phone-board");
    await pp.getByRole("button", { name: "Open navigation" }).click();
    await pp.waitForTimeout(600);
    await pp.screenshot({ path: path.join(dir, "phone-explorer.png") });
    await phone.close();
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
