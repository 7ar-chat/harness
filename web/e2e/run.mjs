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
    HARNESS_MODEL_PRICES: "off", // no download of the public price table
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
  const visible = (loc, what, timeout = T) => loc.first().waitFor({ state: "visible", timeout }).catch(() => Promise.reject(new Error(`expected visible: ${what}`)));
  const gone = (loc, what, timeout = T) => loc.first().waitFor({ state: "hidden", timeout }).catch(() => Promise.reject(new Error(`expected gone: ${what}`)));
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

  async function step(name, fn) {
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
    await visible(page.getByRole("dialog").getByRole("link", { name: /agent's session/ }), "the link to the agent's session");
  });

  await step("g4. the inbox lists a session that waits for an approval and clears when it is approved", async () => {
    await page.goto(`${base}/#/inbox`);
    await visible(page.getByText("Nothing waits for you"), "the empty inbox");
    const id = await newSession("E2E inbox");
    await send("please run the script");
    await visible(page.getByRole("alertdialog", { name: "Approval needed" }), "the approval card");
    await page.goto(`${base}/#/inbox`);
    await visible(page.getByText("E2E inbox"), "the waiting session in the inbox");
    eq(await page.getByText("Nothing waits for you").count(), 0, "the inbox is not empty");
    await page.getByText("E2E inbox").first().click();
    await page.waitForURL(new RegExp(`#/t/${id}$`));
    const card = page.getByRole("alertdialog", { name: "Approval needed" });
    await visible(card, "the approval card after opening the session");
    await card.getByRole("button", { name: "Approve", exact: true }).click();
    await visible(page.getByText("The script finished."), "the answer after approval");
    await idle(id);
    await page.goto(`${base}/#/inbox`);
    await visible(page.getByText("Nothing waits for you"), "the inbox is empty again");
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
