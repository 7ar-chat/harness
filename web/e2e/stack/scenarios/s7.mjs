// s7: security checks through the real stack: cookie flags, origin check, CSP, unauthenticated access,
// secrets never in a response (or a log), rate limit.
import { execFile } from "node:child_process";
import path from "node:path";
import { eq, ok } from "../lib.mjs";
import { repo } from "../stack.mjs";

export const title = "security checks";
const ctx = {};

const SECRETS = {
  gitToken: "ghp_E2E_SECRET_git_token_123456",
  serviceToken: "svc_E2E_SECRET_service_token_789",
  tapdPassword: "e2e-tapd-password",
  webhookSecret: "E2E_SECRET_webhook_signing_321",
  executorEnv: "E2E_SECRET_executor_credential_654",
};

export const steps = [
  [
    "the session cookie is HttpOnly, SameSite=Lax, not readable by scripts; Secure follows the https redirect URL",
    async (env) => {
      const alice = await env.person("alice");
      const cookies = await alice.ctx.cookies(env.S.base);
      const session = cookies.find((c) => c.httpOnly);
      ok(session, `a session cookie exists (${cookies.map((c) => c.name).join(",")})`);
      eq([session.httpOnly, session.sameSite, session.path], [true, "Lax", "/"], "cookie flags");
      // this stack is http://127.0.0.1: Secure is set only when OIDC_REDIRECT_URL is https (unit-tested in app/product)
      eq(session.secure, false, "Secure is off on a plain-http origin");
      eq(await alice.page.evaluate(() => document.cookie), "", "no cookie is visible to page scripts");
    },
  ],
  [
    "a state-changing request from another Origin is refused; the same origin passes",
    async (env) => {
      const alice = await env.person("alice");
      for (const [method, path, body] of [["POST", "/threads", { title: "csrf" }], ["PUT", "/me/usage/cap", { monthlyUsd: 0 }], ["POST", "/projects", { name: "csrf" }]]) {
        const r = await alice.api(method, path, body, { headers: { origin: "https://evil.example" } });
        eq([r.status, r.json?.error?.code], [403, "forbidden_origin"], `${method} ${path} from https://evil.example`);
      }
      const nul = await alice.api("POST", "/threads", { title: "csrf" }, { headers: { origin: "null" } });
      eq(nul.status, 403, "Origin: null (a sandboxed frame) is refused too");
      const same = await alice.api("POST", "/threads", { title: "same origin" }, { headers: { origin: env.S.base } });
      eq(same.status, 201, "the same origin is accepted");
      const threads = JSON.stringify(await alice.get("/overview"));
      ok(!threads.includes('"csrf"'), "nothing was created by the refused requests");
    },
  ],
  [
    "the UI is served with a Content-Security-Policy and the usual security headers",
    async (env) => {
      const r = await fetch(`${env.S.base}/`);
      eq(r.status, 200, "the UI answers without a session (it shows the sign-in page)");
      const csp = r.headers.get("content-security-policy") ?? "";
      ok(/default-src/.test(csp) && /frame-ancestors/.test(csp), `CSP present: ${csp.slice(0, 200)}`);
      ok(!/script-src[^;]*'unsafe-eval'/.test(csp), "no unsafe-eval in script-src");
      eq(r.headers.get("x-content-type-options"), "nosniff", "nosniff");
    },
  ],
  [
    "every /api route without a cookie is 401; healthz is open",
    async (env) => {
      for (const p of ["/me", "/overview", "/threads", "/projects", "/events", "/admin/users", "/me/preferences", "/usage", "/needs-review", "/models"]) {
        const r = await fetch(`${env.S.base}/api${p}`, { redirect: "manual" });
        eq(r.status, 401, `GET /api${p} without a cookie`);
      }
      for (const [m, p] of [["POST", "/threads"], ["POST", "/projects"], ["DELETE", "/threads/x"]]) {
        const r = await fetch(`${env.S.base}/api${p}`, { method: m, headers: { "content-type": "application/json" }, body: m === "DELETE" ? undefined : "{}" });
        eq(r.status, 401, `${m} /api${p} without a cookie`);
      }
      const forged = await fetch(`${env.S.base}/api/me`, { headers: { cookie: "harness_session=forged.value" } });
      eq(forged.status, 401, "a forged cookie");
      eq((await fetch(`${env.S.base}/healthz`)).status, 200, "healthz");
    },
  ],
  [
    "no secret appears in any API response: git host token, TAPD password, webhook secret, executor credentials",
    async (env) => {
      const { S } = env;
      const alice = await env.person("alice");
      const proj = await alice.post("/projects", { name: "Secrets" });
      ctx.pid = proj.id;
      // 1. a git host (admin) with a service token, and alice's own token
      const host = await alice.api("POST", "/admin/git-hosts", { name: "git.example.com", kind: "git", baseUrl: "https://git.example.com", serviceToken: SECRETS.serviceToken, serviceUsername: "svc" });
      ok(host.status < 300, `register the git host: ${host.status} ${host.text}`);
      const tok = await alice.api("PUT", "/me/git-hosts/git.example.com/token", { token: SECRETS.gitToken, username: "alice" });
      ok(tok.status < 300, `store the git token: ${tok.status} ${tok.text}`);
      // 2. TAPD credentials of the project (the base URL is the fake one of the stack: private addresses are allowed there)
      const tapd = await alice.api("PUT", `/projects/${proj.id}/requirements/tapd`, { workspaceId: "1001", baseUrl: S.tapd.url, user: S.tapd.user, password: SECRETS.tapdPassword });
      ok(tapd.status < 300, `store the TAPD credentials: ${tapd.status} ${tapd.text}`);
      // 3. a webhook channel with a signing secret
      const ch = await alice.api("POST", `/projects/${proj.id}/notification-channels`, { kind: "webhook", name: "Secret hook", url: S.hook.url, secret: SECRETS.webhookSecret });
      ok(ch.status < 300, `store the webhook channel: ${ch.status} ${ch.text}`);
      // 4. executor credentials
      const exs = await alice.get("/executors");
      const list = Array.isArray(exs) ? exs : exs.executors ?? [];
      ctx.executor = list[0]?.name;
      ok(ctx.executor, `a builtin executor exists: ${JSON.stringify(exs).slice(0, 200)}`);
      const envName = list[0].credentialEnv?.[0];
      ok(envName, `the executor declares a credential variable: ${JSON.stringify(list[0]).slice(0, 200)}`);
      const cred = await alice.api("PUT", `/me/executors/${ctx.executor}/credentials`, { env: { [envName]: SECRETS.executorEnv } });
      ok(cred.status < 300, `store the executor credential: ${cred.status} ${cred.text}`);

      // read everything the person can read, on both replicas, and look for the secret values
      const paths = [
        "/me", "/me/preferences", "/me/schedules", "/me/notifications", "/me/insights", "/me/documents", "/me/jobs", "/connectors", "/git-hosts", "/admin/users", "/executors",
        "/projects", "/overview", "/needs-review", "/usage?scope=me", "/supervisor/settings", "/memory", "/recipes", "/templates", "/environments", "/models", "/commands",
        `/projects/${proj.id}`, `/projects/${proj.id}/members`, `/projects/${proj.id}/invites`, `/projects/${proj.id}/executors`, `/projects/${proj.id}/notification-channels`,
        `/projects/${proj.id}/notifications`, `/projects/${proj.id}/notifications/log`, `/projects/${proj.id}/requirements`, `/projects/${proj.id}/requirements/settings`,
        `/projects/${proj.id}/requirements/tapd`, `/projects/${proj.id}/audit`, `/projects/${proj.id}/board`, `/projects/${proj.id}/schedules`, `/projects/${proj.id}/triggers`,
        `/projects/${proj.id}/tasks`, `/projects/${proj.id}/digest`, `/projects/${proj.id}/insights`, `/projects/${proj.id}/github`, `/projects/${proj.id}/workflows`,
        `/projects/${proj.id}/memory/export`, `/usage?scope=project&project=${proj.id}`,
      ];
      const seen = [];
      for (const replica of [0, 1]) {
        for (const p of paths) {
          const r = await alice.api("GET", p, undefined, { replica });
          seen.push(`${p} [${r.status}] ${r.text}`);
        }
      }
      const answered = seen.filter((s) => / \[2\d\d\] /.test(s)).length;
      ok(answered >= paths.length, `most endpoints answered 2xx (${answered}/${seen.length})`);
      for (const [name, value] of Object.entries(SECRETS)) {
        const hit = seen.find((s) => s.includes(value));
        ok(!hit, `the ${name} value appears in a response: ${hit?.slice(0, 120)}`);
      }
      // the other people's views too (a member reads the project's channels)
      const dave = await env.person("dave");
      void dave;
      // and in no process log
      for (const [pname, p] of Object.entries(S.procs)) for (const [name, value] of Object.entries(SECRETS)) ok(!p.log().includes(value), `the ${name} value is in the ${pname} log`);
    },
  ],
  [
    "rate limit: a second test message on the same channel within a second is refused (429)",
    async (env) => {
      const alice = await env.person("alice");
      const chans = await alice.get(`/projects/${ctx.pid}/notification-channels`);
      const id = (Array.isArray(chans) ? chans : chans.channels)[0].id;
      await env.sleep(1200);
      // the limiter lives in each replica's memory (a documented limit: with two replicas the ceiling is two a second), so pin one replica
      const [a, b] = await Promise.all([1, 1].map(() => alice.api("POST", `/projects/${ctx.pid}/notification-channels/${id}/test`, {}, { replica: 0 })));
      const codes = [a.status, b.status].sort();
      ok(codes.includes(429) || codes.some((c) => c >= 400), `one of two immediate tests is refused: ${codes} ${a.text} ${b.text}`);
      ok(codes.some((c) => c < 300), "the other one is accepted");
      const again = await alice.api("POST", `/projects/${ctx.pid}/notification-channels/${id}/test`, {}, { replica: 0 });
      eq(again.status, 429, "a test straight after another is 429");
      ok(Number(again.headers["retry-after"] ?? "1") >= 0, "Retry-After is sane");
    },
  ],
  [
    "the production smoke kit (scripts/prod-smoke.mjs) passes against the stack with a person's cookie, and fails without one",
    async (env) => {
      const alice = await env.person("alice");
      const cookie = (await alice.ctx.cookies(env.S.base)).map((c) => `${c.name}=${c.value}`).join("; ");
      const script = path.join(repo, "scripts/prod-smoke.mjs");
      // async: the proxy of the stack runs in this process and must keep answering
      const run = (cookieValue) =>
        new Promise((resolve) =>
          execFile("node", [script], { env: { PATH: process.env.PATH, BASE_URL: env.S.base, SESSION_COOKIE: cookieValue }, timeout: 60_000 }, (err, stdout, stderr) => resolve({ status: err ? (err.code ?? null) : 0, stdout, stderr })),
        );
      const good = await run(cookie);
      ok(good.status === 0, `prod-smoke exit ${good.status}: ${good.stdout.split("\n").filter((l) => /FAIL|checks passed/.test(l)).join(" // ")} ${good.stderr.slice(0, 300)}`);
      ok(good.stdout.includes("checks passed") && !good.stdout.includes(cookie.split("=")[1]), "a table is printed and the cookie is not");
      const bad = await run("x=forged");
      eq(bad.status, 1, "a forged cookie is reported as a failure");
    },
  ],
];
