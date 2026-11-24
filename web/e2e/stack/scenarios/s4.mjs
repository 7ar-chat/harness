// s4: the worker path: a run queued by replica 0 is executed by the worker and streams to a browser on replica 1;
// the worker is killed mid-run (SIGTERM with grace, SIGKILL) and the run is not lost.
import { eq, ok } from "../lib.mjs";
import { poll, sleep } from "../stack.mjs";

export const title = "worker path, worker kill";
const ctx = {};

const metric = async (S, name, label = "") => {
  const text = await (await fetch(`${S.workerAdmin}/metrics`)).text();
  const line = text.split("\n").find((l) => l.startsWith(name + label) || (label === "" && l.startsWith(name + " ")));
  return line ? Number(line.split(" ").pop()) : 0;
};
const finished = async (S) => {
  const text = await (await fetch(`${S.workerAdmin}/metrics`)).text();
  return text.split("\n").filter((l) => l.startsWith("harness_worker_runs_finished_total{")).map((l) => `${l.match(/status="(\w+)"/)?.[1]}=${l.split(" ").pop()}`);
};

/** The AG-UI events of a run as a raw SSE stream, served by a pinned replica. */
async function runEvents(env, person, runId, replica) {
  const cookies = await person.ctx.cookies(env.S.base);
  const ac = new AbortController();
  const r = await fetch(`${env.S.base}/api/runs/${runId}/events`, {
    headers: { cookie: cookies.map((c) => `${c.name}=${c.value}`).join("; "), accept: "text/event-stream", "x-pin-replica": String(replica) },
    signal: ac.signal,
  });
  const out = { status: r.status, replica: r.headers.get("x-replica"), text: "", close: () => ac.abort() };
  (async () => {
    try {
      for await (const c of r.body) out.text += new TextDecoder().decode(c);
    } catch {}
  })();
  return out;
}

export const steps = [
  [
    "a run queued on replica 0 is executed by the worker; its events stream to a browser on replica 1",
    async (env) => {
      const { S } = env;
      const alice = await env.person("alice");
      const before = await metric(S, "harness_worker_runs_started_total");
      const th = await alice.post("/threads", { title: "Worker path" }, { replica: 0 });
      ctx.thread = th;
      const run = await alice.post(`/threads/${th.id}/runs`, { message: "hello worker", model: "gpt-test" }, { replica: 0 });
      const ev = await runEvents(env, alice, run.id, 1);
      eq([ev.status, ev.replica], [200, "1"], "the event stream is served by replica 1");
      await env.idle(alice, th.id);
      await poll(() => ev.text.includes("RUN_FINISHED"), "RUN_FINISHED on the stream of replica 1", 10_000);
      ok(ev.text.includes("TEXT_MESSAGE") && /fake/.test(ev.text), "the model's text streamed");
      ev.close();
      eq(await metric(S, "harness_worker_runs_started_total"), before + 1, "the worker started exactly this run");
      ok((await finished(S)).some((x) => x.startsWith("completed=") || x.startsWith("succeeded=") || x.startsWith("done=")), `the worker finished it: ${await finished(S)}`);
      const rec = await alice.get(`/runs/${run.id}`);
      eq(rec.status, "completed", "run record");
    },
  ],
  [
    "SIGTERM mid-run: the worker drains within the grace; the run is interrupted, not lost, and goes on after the restart",
    async (env) => {
      const { S } = env;
      const alice = await env.person("alice");
      const th = await alice.post("/threads", { title: "Worker SIGTERM" });
      const run = await alice.post(`/threads/${th.id}/runs`, { message: "slow reply please", model: "gpt-test" });
      await S.fake.held(); // the model's reply is held before its last piece: the run is in flight on the worker
      eq(await metric(S, "harness_worker_runs_in_flight"), 1, "one run in flight on the worker");
      const w = S.procs.worker;
      const killed = w.kill("SIGTERM", 30_000);
      await poll(async () => (await fetch(`${S.workerAdmin}/readyz`).catch(() => ({ status: 503 }))).status === 503, "readyz 503 while draining", 3000, 100).catch(() => {});
      await killed;
      S.fake.release();
      const after = await alice.get(`/runs/${run.id}`);
      console.log("  after SIGTERM:", after.status, JSON.stringify(after).slice(0, 250));
      eq([after.status, after.errorCode], ["interrupted", "interrupted"], "the run is marked interrupted (never lost, never hanging)");
      const t = await alice.get(`/threads/${th.id}`);
      eq(t.status, "idle", "the thread is idle again, so the person can go on");
      ok(t.messages.some((m) => m.role === "assistant" && /Slow reply/.test(String(m.content))), "the text streamed before the stop is kept");
      await S.start.worker();
      // "Send your message again to continue": the restarted worker takes the new run
      const again = await alice.post(`/threads/${th.id}/runs`, { message: "hello again", model: "gpt-test" });
      await env.idle(alice, th.id);
      eq((await alice.get(`/runs/${again.id}`)).status, "completed", "the run sent after the restart completes on the new worker");
    },
  ],
  [
    "SIGKILL mid-run: after the restart the sweeper ends or re-queues the orphaned run",
    async (env) => {
      const { S } = env;
      const alice = await env.person("alice");
      const th = await alice.post("/threads", { title: "Worker SIGKILL" });
      const run = await alice.post(`/threads/${th.id}/runs`, { message: "slow reply please", model: "gpt-test" });
      await S.fake.held();
      await S.procs.worker.kill("SIGKILL");
      S.fake.release();
      await sleep(500);
      await S.start.worker();
      // the heartbeat of the dead worker expires (10 s) and the sweeper (every 30 s) ends the orphan: ~1 minute
      const final = await poll(async () => {
        const r = await alice.get(`/runs/${run.id}`);
        return ["completed", "failed", "interrupted", "cancelled"].includes(r.status) ? r : null;
      }, "the orphaned run is ended by the sweeper", 90_000, 500);
      eq([final.status, final.errorCode], ["interrupted", "interrupted"], "the orphan is interrupted by the sweeper");
      eq((await alice.get(`/threads/${th.id}`)).status, "idle", "the thread is idle again");
      const again = await alice.post(`/threads/${th.id}/runs`, { message: "hello after kill -9", model: "gpt-test" });
      await env.idle(alice, th.id);
      eq((await alice.get(`/runs/${again.id}`)).status, "completed", "a new run completes on the restarted worker");
    },
  ],
];
