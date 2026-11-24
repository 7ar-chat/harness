import { startStack } from "./stack.mjs";
const S = await startStack();
try {
  for (const p of ["/healthz", "/auth/providers", "/api/me"]) {
    const r = await fetch(S.base + p);
    console.log(p, r.status, r.headers.get("x-replica"), (await r.text()).slice(0, 200));
  }
  console.log((await (await fetch(S.workerAdmin + "/metrics")).text()).split("\n").filter((l) => l.startsWith("harness_worker")).join("\n"));
} finally {
  await S.stop();
}
