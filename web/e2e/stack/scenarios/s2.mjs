// s2: sharing and permissions across people and replicas.
import { eq, ok } from "../lib.mjs";
import { openFeed } from "../feed.mjs";
import { poll } from "../stack.mjs";

export const title = "sharing and permissions";
const ctx = {}; // state shared by the steps of this scenario

export const steps = [
  [
    "alice creates a project and invites bob (member) and dave (viewer) by email; invites are claimed at first login",
    async (env) => {
      const alice = await env.person("alice");
      ctx.project = await alice.post("/projects", { name: "Shared work" });
      ok(ctx.project.id, "project id");
      // dave has never signed in: the invite waits for his verified email
      const inv1 = await alice.api("POST", `/projects/${ctx.project.id}/invites`, { email: "bob@example.com", role: "member" });
      const inv2 = await alice.api("POST", `/projects/${ctx.project.id}/invites`, { email: "dave@example.com", role: "viewer" });
      eq([inv1.status < 300, inv2.status < 300], [true, true], `invites accepted (${inv1.text} ${inv2.text})`);
      const bob = await env.person("bob");
      const dave = await env.person("dave"); // first login: claims the invite, then alice approves the sign-up
      const members = await alice.get(`/projects/${ctx.project.id}/members`);
      eq(members.map((m) => `${m.login}:${m.role}`).sort(), ["alice:owner", "bob:member", "dave:viewer"], "members");
      for (const p of [bob, dave]) {
        const me = await p.get("/me");
        ok(me.projects.some((x) => x.id === ctx.project.id), `${p.name} sees the project`);
      }
    },
  ],
  [
    "bob creates a session: alice sees it appear LIVE (feed through Redis, two different replicas)",
    async (env) => {
      const alice = await env.person("alice");
      const bob = await env.person("bob");
      // alice's raw feed is served by replica 1, bob's write by replica 0
      const feed = await openFeed(env, alice, 1);
      eq(feed.replica, "1", "alice's stream served by replica 1");
      const bobFeed = await openFeed(env, bob, 0);
      eq(bobFeed.replica, "0", "bob's stream served by replica 0");
      await alice.page.goto(`${env.S.base}/#/p/${ctx.project.id}`);
      const t = await bob.post("/threads", { title: "Bob live session", projectId: ctx.project.id }, { replica: 0 });
      ctx.thread = t;
      const ev = await poll(() => feed.events.find((e) => JSON.stringify(e).includes(t.id)), "an event about the session on alice's stream", 15_000);
      ok(/thread\.created/.test(JSON.stringify(ev)), `the event is thread.created: ${JSON.stringify(ev).slice(0, 200)}`);
      feed.close();
      bobFeed.close();
      await env.visible(alice.page.getByText("Bob live session").first(), "bob's session in alice's browser", 20_000);
      // every browser stream is logged with the replica that served it; across all streams both replicas served some
      eq(env.replicasOfEvents(0).sort(), [0, 1], "both replicas served /api/events streams");
    },
  ],
  [
    "bob sees the project's sessions; a personal session of bob is invisible to alice",
    async (env) => {
      const alice = await env.person("alice");
      const bob = await env.person("bob");
      const list = await bob.get("/overview");
      ok(JSON.stringify(list).includes(ctx.thread.id), "bob lists the project session");
      const mine = await bob.post("/threads", { title: "Bob private notes" });
      const aliceList = await alice.get("/overview");
      ok(!JSON.stringify(aliceList).includes(mine.id), "alice does not list bob's personal session");
      const direct = await alice.api("GET", `/threads/${mine.id}`);
      eq(direct.status, 404, "alice opening bob's personal session by id");
      const ov = await alice.get("/overview");
      ok(!JSON.stringify(ov).includes(mine.id), "not in alice's overview");
    },
  ],
  [
    "dave (viewer) reads but cannot create sessions or tasks",
    async (env) => {
      const dave = await env.person("dave");
      const read = await dave.api("GET", `/threads/${ctx.thread.id}`);
      eq(read.status, 200, "viewer reads the session");
      const mk = await dave.api("POST", "/threads", { title: "dave tries", projectId: ctx.project.id });
      eq(mk.status, 403, `viewer creates a session: ${mk.text}`);
      const send = await dave.api("POST", `/threads/${ctx.thread.id}/runs`, { message: "hello", model: "gpt-test" });
      eq(send.status, 403, `viewer sends a message: ${send.text}`);
      const task = await dave.api("POST", `/projects/${ctx.project.id}/tasks`, { title: "t", description: "d" });
      eq(task.status, 403, `viewer creates a task: ${task.text}`);
      const inv = await dave.api("POST", `/projects/${ctx.project.id}/invites`, { email: "erin@example.com", role: "member" });
      eq(inv.status, 403, "viewer invites");
    },
  ],
  [
    "erin (not a member) gets 404 for the project and sees nothing in the overview",
    async (env) => {
      const erin = await env.person("erin");
      for (const path of [`/projects/${ctx.project.id}`, `/projects/${ctx.project.id}/members`, `/threads/${ctx.thread.id}`]) {
        const r = await erin.api("GET", path);
        ok([403, 404].includes(r.status), `${path} -> ${r.status}`);
      }
      const ov = await erin.get("/overview");
      ok(!JSON.stringify(ov).includes(ctx.project.id) && !JSON.stringify(ov).includes(ctx.thread.id), "overview has nothing of the project");
      const mk = await erin.api("POST", "/threads", { title: "erin tries", projectId: ctx.project.id });
      ok([403, 404].includes(mk.status), `erin creating in the project -> ${mk.status}`);
    },
  ],
];
