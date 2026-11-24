#!/usr/bin/env node
// Read-only smoke checks of a running 7ar server, with the session of a real person.
//
//   BASE_URL=https://app.example.com SESSION_COOKIE='<name>=<value>' node scripts/prod-smoke.mjs
//
// SESSION_COOKIE is the Cookie header value of a signed-in browser (DevTools > Application > Cookies: the HttpOnly
// session cookie of the site, as `name=value`). Optional: THREAD_ID to use a given session for the per-thread checks
// (default: the first session of GET /api/overview), PROJECT_ID likewise, TIMEOUT_MS (default 15000).
//
// Only GET requests are sent (plus the unauthenticated GET /healthz and one GET without the cookie that must be 401).
// Nothing is created, changed or deleted, and the cookie is never printed. Exit code 0 when every check passed.
const base = (process.env.BASE_URL ?? "").replace(/\/+$/, "");
const cookie = process.env.SESSION_COOKIE ?? "";
const timeout = Number(process.env.TIMEOUT_MS ?? 15000);
if (!/^https?:\/\//.test(base) || !cookie) {
  console.error("usage: BASE_URL=https://host SESSION_COOKIE='name=value' node scripts/prod-smoke.mjs");
  process.exit(2);
}

const rows = [];
async function get(path, { auth = true } = {}) {
  const t0 = Date.now();
  try {
    const r = await fetch(base + path, { headers: { accept: "application/json", ...(auth ? { cookie } : {}) }, redirect: "manual", signal: AbortSignal.timeout(timeout) });
    const text = await r.text();
    let json = null;
    try {
      json = JSON.parse(text);
    } catch {}
    return { status: r.status, json, text, ms: Date.now() - t0 };
  } catch (e) {
    return { status: 0, json: null, text: String(e.message ?? e), ms: Date.now() - t0 };
  }
}
/** One row: `want` is a status, a list of statuses or "2xx". */
async function check(area, path, want = "2xx", o = {}) {
  const r = await get(path, o);
  const okStatus = want === "2xx" ? r.status >= 200 && r.status < 300 : [].concat(want).includes(r.status);
  rows.push({ area, path: path.length > 52 ? path.slice(0, 51) + "..." : path, want: [].concat(want).join("|"), got: r.status || "ERR", ms: r.ms, ok: okStatus });
  return r;
}

// ---- no session needed ---------------------------------------------------------------------------------------
await check("health", "/healthz", 200, { auth: false });
await check("auth", "/api/me", 401, { auth: false });
await check("auth", "/auth/providers", 200, { auth: false });

// ---- the person ----------------------------------------------------------------------------------------------
const me = await check("me", "/api/me");
if (me.status !== 200) {
  print();
  console.error("\nthe session cookie was not accepted (or the account is not approved): stopping");
  process.exit(1);
}
const login = me.json?.user?.login ?? "?";
await check("me", "/api/me/preferences");
await check("me", "/api/me/schedules");
await check("me", "/api/me/notifications");
await check("me", "/api/me/insights");
await check("me", "/api/me/documents", [200, 404]); // optional feature
await check("me", "/api/me/jobs", [200, 404]); // optional feature
await check("me", "/api/connectors");
await check("me", "/api/git-hosts", [200, 404]);
await check("me", "/api/executors", [200, 404]);
await check("me", "/api/usage?scope=me");
await check("me", "/api/memory", [200, 404]);
await check("me", "/api/needs-review");
await check("me", "/api/supervisor/settings", [200, 404]);

// ---- global lists ---------------------------------------------------------------------------------------------
const ov = await check("overview", "/api/overview");
for (const p of ["/api/models", "/api/environments", "/api/commands", "/api/projects", "/api/templates", "/api/recipes", "/api/workflow-templates"]) await check("lists", p, [200, 404]);
if (me.json?.admin) await check("admin", "/api/admin/users?status=pending");

// ---- one project: every GET area ------------------------------------------------------------------------------
const projects = me.json?.projects ?? [];
const pid = process.env.PROJECT_ID ?? projects[0]?.id;
if (pid) {
  const P = `/api/projects/${pid}`;
  await check("project", P);
  for (const sub of ["members", "invites", "executors", "notification-channels", "notifications", "notifications/log", "requirements", "requirements/settings", "requirements/tapd",
    "audit", "board", "schedules", "triggers", "tasks", "digest", "insights", "github", "workflows", "workflow-runs", "memory/export", "factory/report", "factory/learning"]) {
    await check("project", `${P}/${sub}`, [200, 404]); // 404: the feature is not configured on this server
  }
  await check("project", `/api/usage?scope=project&project=${pid}`);
} else rows.push({ area: "project", path: "(this person has no project: area skipped)", want: "-", got: "-", ms: 0, ok: true });

// ---- one thread: queue, published artifacts, environment, usage, files ----------------------------------------------
let tid = process.env.THREAD_ID;
if (!tid) {
  const j = JSON.stringify(ov.json ?? {});
  tid = /"threads?":\s*\[\s*\{[^}]*?"id":\s*"([0-9a-f-]{36})"/.exec(j)?.[1] ?? /"id":\s*"([0-9a-f-]{36})"/.exec(j)?.[1];
}
if (tid) {
  const T = `/api/threads/${tid}`;
  await check("thread", T);
  for (const sub of ["queue", "published", "environment", "usage"]) await check("thread", `${T}/${sub}`, [200, 404]);
  await check("thread", `${T}/files`, [200, 404, 503]); // 503: the session's sandbox is asleep or not reachable from this server
} else rows.push({ area: "thread", path: "(no session found: area skipped)", want: "-", got: "-", ms: 0, ok: true });

// ---- the UI shell and its security headers (no cookie) ----------------------------------------------------------------
{
  const r = await fetch(base + "/", { redirect: "manual", signal: AbortSignal.timeout(timeout) }).catch(() => null);
  const csp = r?.headers.get("content-security-policy") ?? "";
  rows.push({ area: "ui", path: "/ (CSP header)", want: "200 + CSP", got: r ? `${r.status}${csp ? " + CSP" : ""}` : "ERR", ms: 0, ok: !!r && r.status === 200 && csp.includes("default-src") });
}

function print() {
  const w = (k) => Math.max(k.length, ...rows.map((r) => String(r[k]).length));
  const cols = ["area", "path", "want", "got", "ms", "ok"];
  const widths = Object.fromEntries(cols.map((c) => [c, w(c)]));
  const line = (r) => cols.map((c) => String(r[c] === true ? "ok" : r[c] === false ? "FAIL" : r[c]).padEnd(widths[c])).join("  ");
  console.log(line(Object.fromEntries(cols.map((c) => [c, c]))));
  console.log(cols.map((c) => "-".repeat(widths[c])).join("  "));
  for (const r of rows) console.log(line(r));
}
console.log(`prod-smoke: ${base} as ${login}\n`);
print();
const bad = rows.filter((r) => !r.ok);
console.log(`\n${rows.length - bad.length}/${rows.length} checks passed${bad.length ? `; failed: ${bad.map((r) => r.path).join(", ")}` : ""}`);
process.exit(bad.length ? 1 : 0);
