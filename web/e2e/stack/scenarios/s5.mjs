// s5: the scheduler, spend caps and the weekly digest across two replicas.
import crypto from "node:crypto";
import { eq, ok } from "../lib.mjs";
import { poll, sleep } from "../stack.mjs";

export const title = "scheduler, caps, digest across replicas";
const ctx = {};

export const steps = [
  [
    "a schedule due every minute is claimed exactly once per minute across the two replicas",
    async (env) => {
      const alice = await env.person("alice");
      ctx.sched = await alice.post("/projects", { name: "Scheduled" });
      const made = await alice.post(`/projects/${ctx.sched.id}/schedules`, { name: "Every minute", prompt: "hello from the schedule", cron: "* * * * *", timezone: "UTC", model: "gpt-test", policy: "readonly", enabled: true });
      ctx.sid = made.id;
      ok(ctx.sid, `schedule id: ${JSON.stringify(made).slice(0, 200)}`);
      // three minute boundaries pass; both replicas tick every few seconds
      const runs = await poll(async () => {
        const r = await alice.get(`/projects/${ctx.sched.id}/schedules/${ctx.sid}/runs`);
        const list = Array.isArray(r) ? r : r.runs ?? [];
        return list.length >= 2 ? list : null;
      }, "two scheduled runs", 150_000, 2000);
      await sleep(8000); // time for a duplicate claim to show up
      const r = await alice.get(`/projects/${ctx.sched.id}/schedules/${ctx.sid}/runs`);
      const list = Array.isArray(r) ? r : r.runs ?? [];
      const minutes = list.map((x) => String(x.scheduledFor ?? x.slot ?? x.startedAt ?? x.createdAt ?? "").slice(0, 16));
      eq(new Set(minutes).size, minutes.length, `one run per minute (runs at ${minutes.join(", ")})`);
      ok(runs.length >= 2, "at least two runs");
      // every scheduled run became exactly one thread run (no hidden duplicates)
      const ov = await alice.get("/overview");
      const titles = JSON.stringify(ov).match(/Every minute/g)?.length ?? 0;
      ok(titles <= list.length + 1, `no more sessions (${titles}) than runs (${list.length})`);
      await alice.patch(`/projects/${ctx.sched.id}/schedules/${ctx.sid}`, { enabled: false });
    },
  ],
  [
    "a project spend cap is enforced on both replicas, and a worker-run run is booked in the ledger",
    async (env) => {
      const alice = await env.person("alice");
      const proj = await alice.post("/projects", { name: "Capped" });
      const th = await alice.post("/threads", { title: "Capped work", projectId: proj.id });
      const first = await alice.post(`/threads/${th.id}/runs`, { message: "first call", model: "gpt-test" });
      await env.idle(alice, th.id);
      // the run executed in the worker; its cost is in the ledger the replicas read ($0.015 per scripted call)
      const usage = await poll(async () => {
        const u = await alice.get(`/usage?scope=project&project=${proj.id}`);
        return u.cap?.spentUsd > 0 ? u : null;
      }, "the worker-run run is booked in the usage ledger", 15_000).catch(async (e) => {
        console.log("  usage:", JSON.stringify(await alice.get(`/usage?scope=project&project=${proj.id}`)).slice(0, 400));
        throw e;
      });
      ok(usage, "usage");
      await alice.put(`/projects/${proj.id}/usage/cap`, { monthlyUsd: 0.01 });
      for (const replica of [0, 1]) {
        const r = await alice.api("POST", `/threads/${th.id}/runs`, { message: "second call", model: "gpt-test" }, { replica });
        eq([r.status, r.json?.error?.code], [402, "spend_cap_reached"], `replica ${replica} refuses a run over the cap`);
      }
      await alice.put(`/projects/${proj.id}/usage/cap`, { monthlyUsd: 0 });
      const ok2 = await alice.api("POST", `/threads/${th.id}/runs`, { message: "after the cap is removed", model: "gpt-test" });
      ok(ok2.status < 300, `a run is accepted again once the cap is removed (${ok2.status})`);
      await env.idle(alice, th.id);
      ok(first.id, "first run");
    },
  ],
  [
    "the weekly digest 'send one now' reaches a webhook channel exactly once",
    async (env) => {
      const { S } = env;
      const alice = await env.person("alice");
      const proj = await alice.post("/projects", { name: "Digested" });
      const ch = await alice.post(`/projects/${proj.id}/notification-channels`, { kind: "webhook", name: "Digest hook", url: S.hook.url, secret: "e2e-digest-secret" });
      ok(ch.id, `channel: ${JSON.stringify(ch).slice(0, 200)}`);
      const rules = await alice.get(`/projects/${proj.id}/notifications`);
      const events = { ...(rules.rules?.events ?? {}) };
      events["digest.ready"] = { channels: [ch.id], person: false };
      await alice.put(`/projects/${proj.id}/notifications`, { ...rules.rules, events });
      const th = await alice.post("/threads", { title: "Something for the digest", projectId: proj.id });
      await env.run(alice, th.id, "work for the digest");
      await env.idle(alice, th.id);
      S.hook.calls.length = 0;
      // one click on replica 1 (the events of the digest go through the shared bus; both replicas hear them)
      const send = await alice.api("POST", `/projects/${proj.id}/digest/send`, {}, { replica: 1 });
      ok(send.status < 300, `the send was accepted: ${send.status} ${send.text}`);
      await poll(() => S.hook.calls.some((c) => /digest/.test(c.body)), "the digest at the webhook", 20_000);
      await sleep(3000);
      const digests = S.hook.calls.filter((c) => /digest/.test(c.body));
      eq(digests.length, 1, "the webhook received the digest exactly once");
      const c = digests[0];
      eq(c.headers["x-7ar-signature"], "sha256=" + crypto.createHmac("sha256", "e2e-digest-secret").update(c.body).digest("hex"), "signed delivery");
    },
  ],
];
