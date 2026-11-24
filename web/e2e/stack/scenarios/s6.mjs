// s6: data durability: both replicas restart mid-session; the queue, the transcript, published artifacts and
// preferences live in Postgres (and S3), not in a process.
import { eq, ok } from "../lib.mjs";
import { poll } from "../stack.mjs";

export const title = "durability across replica restarts";
const ctx = {};

/** Restart both replicas at once (SIGTERM, as an orchestrator does) and wait until both serve again. */
async function restartReplicas(env) {
  const { S } = env;
  await Promise.all([S.procs.web0.restart("SIGTERM"), S.procs.web1.restart("SIGTERM")]);
  for (const r of S.replicas) await poll(async () => (await fetch(`${r}/healthz`)).ok, `${r} healthy`, 20_000, 200);
}

export const steps = [
  [
    "the queue lives on Postgres: a message queued while a reply runs survives a restart of both replicas and is sent when the reply ends",
    async (env) => {
      const { S } = env;
      const alice = await env.person("alice");
      const page = alice.page;
      await page.goto(`${S.base}/#/`);
      await page.getByRole("button", { name: "New session", exact: true }).click();
      await page.waitForURL(/#\/t\/[^/]+$/);
      ctx.id = page.url().split("/t/")[1];
      const composer = page.getByRole("textbox", { name: "Message" });
      await env.visible(composer, "composer");
      await composer.fill("slow reply please");
      await page.getByRole("button", { name: "Send", exact: true }).click();
      await S.fake.held(); // the reply runs on the worker, held before its last piece
      await env.visible(page.getByText(/Slow reply finally/), "the partial reply");
      await composer.fill("queued alpha message");
      await page.getByRole("button", { name: "Queue", exact: true }).click();
      const queue = page.getByRole("region", { name: "Queued messages" });
      await env.visible(queue.getByText("1 message queued"), "one queued message");
      await restartReplicas(env);
      for (const replica of [0, 1]) {
        const q = await alice.get(`/threads/${ctx.id}/queue`, { replica });
        eq(q.map((x) => x.text), ["queued alpha message"], `the queue as replica ${replica} reads it after the restart`);
      }
      // the reply was running on the worker, not on a replica: it goes on
      S.fake.release();
      await env.visible(page.getByText("Slow reply finally done."), "the held reply completes in the browser after the restart", 30_000);
      await env.visible(page.getByText("Alpha answer."), "the queued message was sent and answered", 30_000);
      eq((await alice.get(`/threads/${ctx.id}/queue`)).length, 0, "the queue is empty");
      await env.idle(alice, ctx.id);
    },
  ],
  [
    "the transcript reloads from Postgres on either replica after another restart",
    async (env) => {
      const alice = await env.person("alice");
      const before = await alice.get(`/threads/${ctx.id}`);
      await restartReplicas(env);
      for (const replica of [0, 1]) {
        const t = await alice.get(`/threads/${ctx.id}`, { replica });
        eq(t.messages.map((m) => `${m.role}:${m.content}`), before.messages.map((m) => `${m.role}:${m.content}`), `the transcript on replica ${replica}`);
      }
      await alice.page.reload();
      await env.visible(alice.page.getByText("Alpha answer.").first(), "the transcript in the reloaded page");
    },
  ],
  [
    "a published artifact survives a restart and is the same on both replicas",
    async (env) => {
      const alice = await env.person("alice");
      const th = await alice.post("/threads", { title: "Artifact durability" });
      await env.run(alice, th.id, "please publish the page");
      await poll(async () => (await alice.get(`/threads/${th.id}/published`)).length === 1, "the artifact is published", 30_000);
      await env.idle(alice, th.id);
      const list = await alice.get(`/threads/${th.id}/published`);
      eq([list[0].name, list[0].version], ["page.html", 1], "the published artifact");
      ok(env.S.s3Objects().length > 0, "the artifact content of a run executed by the worker is in the S3 bucket (RustFS)");
      await restartReplicas(env);
      for (const replica of [0, 1]) {
        const l = await alice.get(`/threads/${th.id}/published`, { replica });
        eq(l.map((x) => [x.artifactId, x.name]), list.map((x) => [x.artifactId, x.name]), `the published list on replica ${replica}`);
      }
    },
  ],
  [
    "preferences follow the person across browsers (the same login in two contexts)",
    async (env) => {
      const bob = await env.person("bob");
      await bob.patch("/me/preferences", { values: { "e2e.stack": { theme: "contrast", n: 7 } } }, { replica: 0 });
      const other = await env.signIn("bob"); // another browser, another cookie jar
      for (const replica of [0, 1]) {
        const p = await other.get("/me/preferences", { replica });
        eq([p.values["e2e.stack"]?.theme, p.values["e2e.stack"]?.n], ["contrast", 7], `bob's preference as read by a second browser on replica ${replica}`);
      }
      await other.ctx.close();
      // a different person does not see it
      const carolLike = await env.person("dave");
      ok(!("e2e.stack" in (await carolLike.get("/me/preferences")).values), "dave does not get bob's preference");
    },
  ],
];
