// Helpers of the stack scenarios: a Person is one browser context (real cookies) signed in through the real Dex form.
import { PASSWORD, poll, sleep } from "./stack.mjs";

export const T = 20_000;

export const eq = (got, want, what) => {
  if (JSON.stringify(got) !== JSON.stringify(want)) throw new Error(`${what}: got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`);
};
export const ok = (cond, what) => {
  if (!cond) throw new Error(`expected: ${what}`);
};

/** What a scenario gets: the stack, the browser, and people. */
export function makeEnv(S, browser, problems) {
  const people = new Map();
  const env = { S, browser, problems, people };

  /** A new browser context (an empty cookie jar) with the console watcher. */
  env.newContext = async () => {
    const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
    const watch = (page) => {
      page.on("pageerror", (e) => problems.push(`pageerror: ${e.message}`));
    };
    ctx.on("page", watch);
    return ctx;
  };

  const personOf = (name, ctx) => {
    const p = { name, ctx, page: null };
    /** An API call with this person's cookies. `replica` pins the replica that serves it (0 or 1). */
    p.api = async (method, path, body, { replica, headers = {}, raw = false } = {}) => {
      const h = { ...headers };
      if (replica !== undefined) h["x-pin-replica"] = String(replica);
      const opt = { method, headers: h, failOnStatusCode: false, maxRedirects: 0 };
      if (body !== undefined) {
        opt.data = body;
        h["content-type"] ??= "application/json";
      }
      const r = await ctx.request.fetch(`${S.base}/api${path}`, opt);
      const text = await r.text();
      let json = null;
      try {
        json = JSON.parse(text);
      } catch {}
      return { status: r.status(), json, text, headers: r.headers(), replica: r.headers()["x-replica"] };
    };
    /** Like api() but fails unless the answer is 2xx; returns the JSON. */
    p.ok = async (method, path, body, o) => {
      const r = await p.api(method, path, body, o);
      if (r.status < 200 || r.status > 299) throw new Error(`${name}: ${method} ${path} -> ${r.status} ${r.text.slice(0, 300)}`);
      return r.json;
    };
    p.get = (path, o) => p.ok("GET", path, undefined, o);
    p.post = (path, body, o) => p.ok("POST", path, body ?? {}, o);
    p.patch = (path, body, o) => p.ok("PATCH", path, body, o);
    p.put = (path, body, o) => p.ok("PUT", path, body, o);
    p.page = null;
    return p;
  };

  /** Sign in through the real UI and the real Dex form. Leaves `p.page` on the app. */
  env.signIn = async (name, ctx = null) => {
    ctx ??= await env.newContext();
    const p = personOf(name, ctx);
    const page = await ctx.newPage();
    p.page = page;
    await page.goto(`${S.base}/`);
    await page.getByRole("link", { name: "Continue with single sign-on" }).click();
    await page.locator('input[name="login"]').fill(`${name}@example.com`);
    await page.locator('input[name="password"]').fill(PASSWORD);
    await page.locator("#submit-login").click();
    await page.waitForURL(new RegExp(`^${S.base}/`), { timeout: T });
    await poll(async () => (await p.api("GET", "/me")).status === 200, `${name} has a session`);
    return p;
  };

  /** A person signed in once for the whole run, approved by alice when the sign-up was pending. */
  env.person = async (name, { welcome = false } = {}) => {
    if (people.has(name)) return people.get(name);
    const p = await env.signIn(name);
    people.set(name, p);
    p.skipWelcome = async () => {
      // the first-run welcome covers the app until it is skipped (s1 checks it, the others skip it)
      const dlg = p.page.getByRole("dialog", { name: "Welcome to 7ar" });
      if (await dlg.waitFor({ state: "visible", timeout: 2500 }).then(() => true, () => false)) await p.page.getByRole("button", { name: "Skip for now" }).click();
    };
    if (!welcome) await p.skipWelcome();
    if (name !== "alice") {
      const me = await p.api("GET", "/me");
      if (me.json?.user?.status === "pending") {
        const alice = await env.person("alice");
        await alice.post(`/admin/users/${name}/approve`);
        await poll(async () => (await p.api("GET", "/me")).json?.user?.status === "approved", `${name} approved`);
        // each replica caches a person's sign-up status for 5 s: wait until both let the person in
        for (const replica of [0, 1]) await poll(async () => (await p.api("GET", "/overview", undefined, { replica })).status === 200, `${name} admitted on replica ${replica}`, 15_000, 250);
      }
    }
    return p;
  };

  /** Log lines of replica streams that served `/api/events` since `since`. */
  env.replicasOfEvents = (since = 0) => [...new Set(S.proxy.log.filter((l) => l.path === "/api/events" && l.t >= since).map((l) => l.replica))];

  // ---- UI helpers ------------------------------------------------------------------------------------------------
  env.visible = (loc, what, timeout = T) => loc.filter({ visible: true }).first().waitFor({ state: "visible", timeout }).catch(() => Promise.reject(new Error(`expected visible: ${what}`)));
  env.gone = (loc, what, timeout = T) => loc.filter({ visible: true }).first().waitFor({ state: "hidden", timeout }).catch(() => Promise.reject(new Error(`expected gone: ${what}`)));

  /** A session of a project for a person, through the API (`message` starts a run). Returns the thread. */
  env.newThread = (p, title, extra = {}) => p.post("/threads", { title, ...extra });
  env.run = (p, threadId, message, extra = {}) => p.post(`/threads/${threadId}/runs`, { message, model: "gpt-test", ...extra });
  env.idle = (p, threadId, timeout = 30_000) =>
    poll(async () => {
      const t = await p.get(`/threads/${threadId}`);
      return ["idle", "done"].includes(t.status) && !t.activeRun;
    }, `thread ${threadId} idle`, timeout, 250);
  env.sleep = sleep;
  return env;
}
