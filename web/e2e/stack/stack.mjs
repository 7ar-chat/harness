// The production-shaped stack of the stack e2e: Postgres + Redis + Dex (OIDC) + RustFS (S3) in Docker (one compose
// project, unique name, random loopback ports), and on the host: TWO harness-web replicas (go build), ONE harness-worker,
// a round-robin reverse proxy (one browser origin) and the scripted fake LLM of web/e2e/fakellm.mjs.
//
// Everything started here is torn down by stack.stop() (also on SIGINT/SIGTERM and on failure).
import { spawn, spawnSync } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import http from "node:http";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { startFakeLlm } from "../fakellm.mjs";

const here = path.dirname(fileURLToPath(import.meta.url));
export const repo = path.resolve(here, "../../..");
const appDir = path.join(repo, "app");

export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/** poll: fn() until it returns a truthy value (returned) or `timeout` ms pass (throws `what`). */
export async function poll(fn, what, timeout = 20_000, every = 100) {
  const end = Date.now() + timeout;
  for (;;) {
    let v;
    try {
      v = await fn();
    } catch (e) {
      if (Date.now() > end) throw new Error(`timed out waiting for: ${what} (${e.message})`);
    }
    if (v) return v;
    if (Date.now() > end) throw new Error(`timed out waiting for: ${what}`);
    await sleep(every);
  }
}

export function freePort() {
  return new Promise((resolve, reject) => {
    const s = net.createServer();
    s.once("error", reject);
    s.listen(0, "127.0.0.1", () => {
      const { port } = s.address();
      s.close(() => resolve(port));
    });
  });
}

export function dockerAvailable() {
  const r = spawnSync("docker", ["info", "--format", "{{.ServerVersion}}"], { encoding: "utf8", timeout: 20_000 });
  return r.status === 0;
}

// bcrypt of "password" (the hash of examples/dev-oidc/dex.yaml)
const PW_HASH = "$2a$10$JPIvRViwR8ixMDJoCGPG5uvu40ZpH515dX1yRenXZ/IAPN/A.q4Zq";
export const PEOPLE = ["alice", "bob", "carol", "dave", "erin"];
export const PASSWORD = "password";

const dexConfig = ({ dexPort, redirect }) => `issuer: http://127.0.0.1:${dexPort}/dex
storage:
  type: memory
web:
  http: 0.0.0.0:5556
oauth2:
  skipApprovalScreen: true
enablePasswordDB: true
staticClients:
  - id: harness
    name: Harness
    secret: harness-e2e-secret
    redirectURIs:
      - ${redirect}
staticPasswords:
${PEOPLE.map(
  (n, i) => `  - email: ${n}@example.com
    hash: "${PW_HASH}"
    username: ${n}
    userID: 08a8684b-db88-4b73-90a9-3cd1661f54${String(60 + i)}`,
).join("\n")}
`;

export async function startStack({ log = console.log, keepLogs = false } = {}) {
  const id = crypto.randomBytes(3).toString("hex");
  const project = `h7e2e${id}`;
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "harness-stack-"));
  const S = { project, tmp, procs: {}, logs: {}, stopped: false, keepLogs };
  fs.mkdirSync(path.join(tmp, "state"));
  fs.mkdirSync(path.join(tmp, "home"));
  fs.mkdirSync(path.join(tmp, "bin"));

  // ---- teardown first: registered before anything is started ---------------------------------------------------
  S.stop = async () => {
    if (S.stopped) return;
    S.stopped = true;
    for (const p of Object.values(S.procs)) await p.kill("SIGKILL").catch(() => {});
    await S.proxy?.close().catch(() => {});
    await S.fake?.close().catch(() => {});
    await S.hook?.close().catch(() => {});
    if (S.composeFile) {
      spawnSync("docker", ["compose", "-p", project, "-f", S.composeFile, "down", "-v", "--remove-orphans", "-t", "2"], { timeout: 90_000, stdio: "ignore" });
      // belt and braces: anything still labelled with the project
      const left = spawnSync("docker", ["ps", "-aq", "--filter", `label=com.docker.compose.project=${project}`], { encoding: "utf8", timeout: 20_000 }).stdout.trim();
      if (left) spawnSync("docker", ["rm", "-f", "-v", ...left.split(/\s+/)], { timeout: 60_000, stdio: "ignore" });
      spawnSync("docker", ["network", "prune", "-f", "--filter", `label=com.docker.compose.project=${project}`], { timeout: 30_000, stdio: "ignore" });
    }
    if (!S.keepLogs) fs.rmSync(tmp, { recursive: true, force: true });
  };

  try {
    // ---- ports ------------------------------------------------------------------------------------------------
    let pgPort, redisPort, dexPort, s3Port, proxyPort, web0Port, web1Port, workerPort, redirect;
    for (let attempt = 1; ; attempt++) {
    // (another process may take a "free" port before docker binds it: a clash is retried with new ports)
    [pgPort, redisPort, dexPort, s3Port, proxyPort, web0Port, web1Port, workerPort] = await Promise.all(Array.from({ length: 8 }, freePort));
    Object.assign(S, { pgPort, redisPort, dexPort, s3Port, proxyPort, web0Port, web1Port, workerPort });
    S.base = `http://127.0.0.1:${proxyPort}`;
    S.replicas = [`http://127.0.0.1:${web0Port}`, `http://127.0.0.1:${web1Port}`];
    S.workerAdmin = `http://127.0.0.1:${workerPort}`;
    redirect = `${S.base}/auth/callback`;

    // ---- docker: postgres, redis, dex, rustfs --------------------------------------------------------------------
    S.composeFile = path.join(tmp, "docker-compose.yml");
    fs.writeFileSync(
      S.composeFile,
      `name: ${project}
services:
  postgres:
    image: postgres:16-alpine
    environment: { POSTGRES_PASSWORD: e2e-pg, POSTGRES_DB: harness }
    ports: ["127.0.0.1:${pgPort}:5432"]
    healthcheck: { test: ["CMD-SHELL", "pg_isready -U postgres -d harness"], interval: 1s, timeout: 3s, retries: 40 }
  redis:
    image: redis:7-alpine
    ports: ["127.0.0.1:${redisPort}:6379"]
    healthcheck: { test: ["CMD", "redis-cli", "ping"], interval: 1s, timeout: 3s, retries: 40 }
  dex:
    image: dexidp/dex:v2.41.1
    command: ["dex", "serve", "/etc/dex/config.yaml"]
    ports: ["127.0.0.1:${dexPort}:5556"]
    configs: [{ source: dexcfg, target: /etc/dex/config.yaml }]
  s3:
    image: rustfs/rustfs:latest
    environment: { RUSTFS_ACCESS_KEY: e2eaccess, RUSTFS_SECRET_KEY: e2esecretkey123, RUSTFS_ADDRESS: ":9000" }
    ports: ["127.0.0.1:${s3Port}:9000"]
    command: ["/data"]
configs:
  dexcfg:
    content: |
${dexConfig({ dexPort, redirect }).replaceAll("$", "$$$$").replace(/^/gm, "      ")}
`,
    );
    log(`docker compose project ${project}: postgres :${pgPort}, redis :${redisPort}, dex :${dexPort}, s3 :${s3Port}`);
    const up = spawnSync("docker", ["compose", "-p", project, "-f", S.composeFile, "up", "-d", "--wait", "--wait-timeout", "120", "postgres", "redis", "dex", "s3"], { encoding: "utf8", timeout: 240_000 });
    if (up.status === 0) break;
    if (attempt < 4 && /address already in use|port is already allocated/.test(up.stderr)) {
      log("a port was taken meanwhile: retrying with new ports");
      spawnSync("docker", ["compose", "-p", project, "-f", S.composeFile, "down", "-v", "--remove-orphans", "-t", "2"], { timeout: 90_000, stdio: "ignore" });
      continue;
    }
    throw new Error(`docker compose up failed:\n${up.stderr || up.stdout}`);
    }
    await poll(async () => (await fetch(`http://127.0.0.1:${dexPort}/dex/.well-known/openid-configuration`)).ok, "dex discovery", 60_000, 300);
    // RustFS: any answer on the port means the server is up (the bucket is created by harness: ARTIFACTS_S3_CREATE_BUCKET)
    await poll(async () => (await fetch(`http://127.0.0.1:${s3Port}/`).catch(() => null)) !== null, "rustfs", 60_000, 300);

    // ---- build ---------------------------------------------------------------------------------------------------
    const uiIndex = path.join(repo, "hosts/httpapi/ui/index.html");
    const newestSrc = (d) => fs.readdirSync(d, { withFileTypes: true }).reduce((m, e) => Math.max(m, e.isDirectory() ? newestSrc(path.join(d, e.name)) : fs.statSync(path.join(d, e.name)).mtimeMs), 0);
    if (!fs.existsSync(uiIndex) || fs.statSync(uiIndex).mtimeMs < newestSrc(path.join(repo, "web/src"))) {
      log("building the web UI (npm run build) ...");
      const b = spawnSync("npm", ["run", "build"], { cwd: path.join(repo, "web"), encoding: "utf8", timeout: 300_000 });
      if (b.status !== 0) throw new Error(`npm run build failed:\n${b.stderr || b.stdout}`);
    }
    log("building harness-web and harness-worker ...");
    for (const [name, pkg] of [["harness-web", "./cmd/harness-web"], ["harness-worker", "./cmd/harness-worker"]]) {
      const b = spawnSync("go", ["build", "-o", path.join(tmp, "bin", name), pkg], { cwd: appDir, encoding: "utf8", timeout: 400_000 });
      if (b.status !== 0) throw new Error(`go build ${name} failed:\n${b.stderr || b.stdout}`);
    }

    // ---- fake LLM, webhook receiver --------------------------------------------------------------------------------
    S.fake = await startFakeLlm();
    S.hook = await startHook();
    const prices = path.join(tmp, "prices.json");
    fs.writeFileSync(prices, JSON.stringify({ "gpt-test": { mode: "chat", litellm_provider: "openai", max_input_tokens: 128000, max_output_tokens: 8000, input_cost_per_token: 1e-3, output_cost_per_token: 1e-3 } }));

    // ---- environment: the SAME for both replicas and the worker ---------------------------------------------------------
    S.secrets = { secretKey: "e2e-secret-key-0123456789abcdef0123456789", sessionKey: crypto.randomBytes(32).toString("hex") };
    S.env = {
      PATH: process.env.PATH,
      HOME: path.join(tmp, "home"),
      TMPDIR: tmp,
      LOCAL_STATE_DIR: path.join(tmp, "state"), // shared by the three processes: one "sandbox host"
      SANDBOX_BACKEND: "local",
      AGENT_PROFILE: "coding",
      HARNESS_ALLOW_LOCAL_SANDBOX: "1",
      HARNESS_MEMORY_EXTRACT: "false",
      MEMORY_IN_CODING: "false",
      LLM_PROVIDER: "openai",
      LLM_BASE_URL: S.fake.url,
      LLM_API_KEY: "test",
      LLM_MODELS: "gpt-test",
      LLM_DEFAULT_MODEL: "gpt-test",
      LLM_STEP_RETRIES: "0",
      LLM_DISCOVER_MODELS: "false",
      HARNESS_MODEL_PRICES: prices,
      POSTGRES_HOST: "127.0.0.1",
      POSTGRES_PORT: String(pgPort),
      POSTGRES_USER: "postgres",
      POSTGRES_PASSWORD: "e2e-pg",
      POSTGRES_DB: "harness",
      POSTGRES_SSLMODE: "disable",
      REDIS_HOST: "127.0.0.1",
      REDIS_PORT: String(redisPort),
      HARNESS_SECRET_KEY: S.secrets.secretKey,
      FACTORY: "1",
      FACTORY_NO_REVIEW: "1",
      FACTORY_RETRY_BACKOFF_SECONDS: "1",
      HARNESS_NOTIFY_ALLOW_PRIVATE: "1",
      RUNTIME_SHUTDOWN_GRACE_SECONDS: "2",
      ARTIFACTS_S3_ENDPOINT: `http://127.0.0.1:${s3Port}`,
      ARTIFACTS_S3_BUCKET: "harness-e2e",
      ARTIFACTS_S3_CREATE_BUCKET: "true",
      AWS_ACCESS_KEY_ID: "e2eaccess",
      AWS_SECRET_ACCESS_KEY: "e2esecretkey123",
    };
    const webEnv = {
      OIDC_ISSUER: `http://127.0.0.1:${dexPort}/dex`,
      OIDC_CLIENT_ID: "harness",
      OIDC_CLIENT_SECRET: "harness-e2e-secret",
      OIDC_REDIRECT_URL: redirect,
      OIDC_LOGIN_CLAIM: "name",
      OIDC_SESSION_KEY: S.secrets.sessionKey,
      OIDC_REQUIRE_APPROVAL: "1",
      HARNESS_ADMINS: "alice",
      HARNESS_WEB_RUNS: "0", // the replicas only queue runs: the worker executes them
    };

    S.start = {
      web: (i, extra = {}) =>
        spawnProc(S, `web${i}`, path.join(tmp, "bin", "harness-web"), ["-team", "-addr", `127.0.0.1:${i === 0 ? web0Port : web1Port}`], { ...S.env, ...webEnv, ...extra }, new RegExp(`harness-web: http://127.0.0.1:${i === 0 ? web0Port : web1Port}/`)),
      worker: (extra = {}) =>
        spawnProc(S, "worker", path.join(tmp, "bin", "harness-worker"), ["-id", "e2e-worker"], { ...S.env, HARNESS_WORKER_ADDR: `127.0.0.1:${workerPort}`, ...extra }, null, async () => (await fetch(`${S.workerAdmin}/readyz`)).ok),
    };
    // Both replicas start at the same moment on an empty database: migrations must be safe to run concurrently.
    await Promise.all([S.start.web(0), S.start.web(1), S.start.worker()]);
    S.proxy = await startProxy(S);
    log(`stack up: ${S.base} (replicas :${web0Port} :${web1Port}, worker admin :${workerPort})`);
    return S;
  } catch (e) {
    S.keepLogs = true; // keep the process logs for the failure report
    S.failure = e;
    await tailAll(S, log);
    await S.stop();
    throw e;
  }
}

/** Print the tail of every process log. */
export async function tailAll(S, log = console.log, lines = 25) {
  for (const [name, p] of Object.entries(S.procs)) {
    const t = p.log().split("\n").filter(Boolean).slice(-lines).join("\n");
    log(`--- ${name} log (tail) ---\n${t}`);
  }
}

/** A process with a captured log, a kill/restart and a readiness probe. */
function spawnProc(S, name, bin, args, env, readyRe, readyFn) {
  const logFile = path.join(S.tmp, `${name}.log`);
  const entry = { name, child: null, buf: "" };
  const start = () =>
    new Promise((resolve, reject) => {
      const child = spawn(bin, args, { env, cwd: S.tmp, stdio: ["ignore", "pipe", "pipe"] });
      entry.child = child;
      entry.exited = new Promise((r) => child.once("exit", (code, sig) => r({ code, sig })));
      let out = "";
      const feed = (d) => {
        const s = String(d);
        out += s;
        entry.buf = (entry.buf + s).slice(-200_000);
        fs.appendFileSync(logFile, s);
        if (readyRe && readyRe.test(out)) resolve();
      };
      child.stdout.on("data", feed);
      child.stderr.on("data", feed);
      const timer = setTimeout(() => reject(new Error(`${name} not ready in 60s:\n${out.slice(-3000)}`)), 60_000);
      child.once("exit", (code) => (clearTimeout(timer), reject(new Error(`${name} exited early (${code}):\n${out.slice(-3000)}`))));
      if (readyFn) {
        poll(readyFn, `${name} ready`, 60_000, 200).then(
          () => (clearTimeout(timer), resolve()),
          () => {},
        );
      } else {
        const orig = resolve;
        resolve = () => (clearTimeout(timer), orig());
      }
    });
  entry.log = () => entry.buf;
  entry.start = start;
  entry.alive = () => entry.child && entry.child.exitCode === null && entry.child.signalCode === null;
  entry.kill = async (sig = "SIGKILL", wait = 15_000) => {
    if (!entry.alive()) return;
    entry.child.kill(sig);
    await Promise.race([entry.exited, sleep(wait)]);
    if (entry.alive()) {
      entry.child.kill("SIGKILL");
      await entry.exited;
    }
  };
  entry.restart = async (sig = "SIGTERM") => {
    await entry.kill(sig);
    await start();
  };
  S.procs[name] = entry;
  return start();
}

/** A webhook receiver on loopback: records what it is sent. */
async function startHook() {
  const hook = { calls: [], code: 200 };
  const srv = http.createServer((req, res) => {
    const chunks = [];
    req.on("data", (c) => chunks.push(c));
    req.on("end", () => {
      hook.calls.push({ url: req.url, headers: req.headers, body: Buffer.concat(chunks).toString("utf8") });
      res.writeHead(hook.code).end();
    });
  });
  await new Promise((r) => srv.listen(0, "127.0.0.1", r));
  hook.url = `http://127.0.0.1:${srv.address().port}/e2e-hook`;
  hook.close = () => new Promise((r) => (srv.closeAllConnections?.(), srv.close(r)));
  return hook;
}

/**
 * The reverse proxy: ONE origin for the browser, round-robin over the replicas (no stickiness: the requests of one
 * browser alternate between them). Adds `X-Replica: <index>` to every response; a request with `X-Pin-Replica: <i>`
 * goes to that replica (tests that need to say "write on A, read on B"). `proxy.log` records every request's replica.
 * A replica that is down answers 502 for that request only (the next request tries the other one).
 */
async function startProxy(S) {
  let n = 0;
  const proxy = { log: [], requests: 0, down: new Set() };
  const srv = http.createServer((req, res) => {
    let i;
    const pin = req.headers["x-pin-replica"];
    if (pin !== undefined) i = Number(pin);
    else {
      i = n++ % 2;
      if (proxy.down.has(i)) i = 1 - i;
    }
    delete req.headers["x-pin-replica"];
    const target = new URL(S.replicas[i]);
    proxy.requests++;
    const out = http.request(
      { host: target.hostname, port: target.port, method: req.method, path: req.url, headers: req.headers },
      (r) => {
        proxy.log.push({ t: Date.now(), method: req.method, path: req.url.split("?")[0], replica: i, status: r.statusCode });
        if (proxy.log.length > 20000) proxy.log.splice(0, 10000);
        res.writeHead(r.statusCode, { ...r.headers, "x-replica": String(i) });
        r.pipe(res);
        res.on("close", () => r.destroy());
      },
    );
    out.on("error", () => {
      if (!res.headersSent) res.writeHead(502, { "x-replica": String(i) }).end("bad gateway (replica down)");
      else res.destroy();
    });
    req.pipe(out);
    res.on("close", () => out.destroy());
  });
  // SSE and long requests must not be cut by a keep-alive timeout
  srv.keepAliveTimeout = 120_000;
  await new Promise((r) => srv.listen(S.proxyPort, "127.0.0.1", r));
  proxy.close = () => new Promise((r) => (srv.closeAllConnections?.(), srv.close(r)));
  return proxy;
}
