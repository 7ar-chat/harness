// s1: sign-in through real OIDC, the approval flow, logout, session expiry.
import { eq, ok, T } from "../lib.mjs";
import { poll } from "../stack.mjs";

export const title = "sign-in, approval, logout, expiry";

export const steps = [
  [
    "alice (admin) signs in, is approved, sees the app and the first-run welcome",
    async (env) => {
      const alice = await env.person("alice");
      const me = await alice.get("/me");
      eq(me.user.login, "alice", "login claim");
      eq(me.user.status, "approved", "admin approved at once");
      ok(me.admin === true, "alice is an administrator (HARNESS_ADMINS)");
      await env.visible(alice.page.getByRole("dialog", { name: "Welcome to 7ar" }), "the first-run welcome");
      await alice.page.keyboard.press("Escape");
      await env.gone(alice.page.getByRole("dialog", { name: "Welcome to 7ar" }), "welcome closed");
      const ck = (await alice.ctx.cookies()).filter((c) => c.name.toLowerCase().includes("session") || c.httpOnly);
      ok(ck.length > 0, "a session cookie was set");
    },
  ],
  [
    "bob signs in and lands on the pending-approval screen",
    async (env) => {
      const bob = await env.signIn("bob");
      env.people.set("bob", bob);
      eq((await bob.get("/me")).user.status, "pending", "bob pending (OIDC_REQUIRE_APPROVAL)");
      await env.visible(bob.page.getByText("Waiting for approval"), "pending screen");
      // every other route is refused while pending
      const r = await bob.api("GET", "/overview");
      eq([r.status, r.json?.error?.code], [403, "pending_approval"], "overview while pending");
    },
  ],
  [
    "alice approves bob in the Admin page, bob reloads and gets in",
    async (env) => {
      const alice = await env.person("alice");
      const bob = await env.person("bob");
      await alice.page.goto(`${env.S.base}/#/admin`);
      const row = alice.page.locator("li", { hasText: "bob@example.com" });
      await env.visible(row, "bob in the pending list");
      await row.getByRole("button", { name: "Approve" }).click();
      await poll(async () => (await bob.get("/me")).user.status === "approved", "bob approved");
      await bob.page.reload();
      await env.gone(bob.page.getByText("Waiting for approval"), "pending screen gone");
      await env.visible(bob.page.getByRole("dialog", { name: "Welcome to 7ar" }), "the first-run welcome opens for bob too");
      await bob.page.getByRole("button", { name: "Skip for now" }).click();
      await env.visible(bob.page.getByRole("button", { name: "New session", exact: true }), "bob sees the app");
    },
  ],
  [
    "carol is rejected by alice and sees the rejected screen",
    async (env) => {
      const carol = await env.signIn("carol");
      env.people.set("carol", carol);
      eq((await carol.get("/me")).user.status, "pending", "carol pending");
      const alice = await env.person("alice");
      await alice.post("/admin/users/carol/reject");
      await carol.page.reload();
      await env.visible(carol.page.getByText("Access denied"), "rejected screen");
      eq((await carol.api("GET", "/overview")).status, 403, "rejected user is refused");
      // a rejected account stays rejected when it signs in again
      const again = await env.signIn("carol");
      eq((await again.get("/me")).user.status, "rejected", "still rejected");
      await again.ctx.close();
    },
  ],
  [
    "logout ends the session: a later request is 401 and the page shows the sign-in",
    async (env) => {
      const p = await env.signIn("alice"); // a second browser of alice
      eq((await p.api("GET", "/me")).status, 200, "signed in");
      await p.page.goto(`${env.S.base}/`);
      const out = await p.ctx.request.post(`${env.S.base}/auth/logout`, { failOnStatusCode: false });
      ok(out.status() < 400, `logout answered ${out.status()}`);
      eq((await p.api("GET", "/me")).status, 401, "me after logout");
      await p.page.reload();
      await env.visible(p.page.getByText("Sign in", { exact: true }), "sign-in page after logout");
      await p.ctx.close();
    },
  ],
  [
    "a removed cookie ends the session in the open app (harness:unauthorized): 'session expired' page",
    async (env) => {
      const p = await env.signIn("alice");
      await p.page.goto(`${env.S.base}/#/`);
      await env.visible(p.page.getByRole("button", { name: "New session", exact: true }), "the app");
      await p.ctx.clearCookies();
      // the next call the app makes answers 401 (the periodic refetch or a click): the app gives way to the sign-in page
      await p.page.getByRole("button", { name: "New session", exact: true }).click(); // POST /api/threads -> 401
      await env.visible(p.page.getByText("Your session expired. Sign in again to continue."), "the 'session expired' sign-in page");
      await p.ctx.close();
    },
  ],
];
