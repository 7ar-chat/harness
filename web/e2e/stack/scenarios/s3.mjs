// s3: approvals across people: bob's run asks, alice (owner) approves, the run continues in the worker.
import { eq, ok } from "../lib.mjs";
import { poll } from "../stack.mjs";

export const title = "approvals across people";
const ctx = {};

export const steps = [
  [
    "bob's run waits for an approval; alice (owner) sees it in Needs you",
    async (env) => {
      const alice = await env.person("alice");
      const bob = await env.person("bob");
      const dave = await env.person("dave");
      ctx.project = await alice.post("/projects", { name: "Approvals" });
      await alice.post(`/projects/${ctx.project.id}/invites`, { email: "bob@example.com", role: "member" });
      await poll(async () => (await bob.get("/me")).projects.some((x) => x.id === ctx.project.id), "bob is a member");
      ctx.thread = await bob.post("/threads", { title: "Bob needs approval", projectId: ctx.project.id });
      const run = await bob.post(`/threads/${ctx.thread.id}/runs`, { message: "please run the script", model: "gpt-test", interactive: true });
      ctx.run = run;
      const waiting = await poll(async () => {
        const t = await bob.get(`/threads/${ctx.thread.id}`);
        return t.pendingApproval ? t : null;
      }, "the thread waits for an approval", 30_000).catch(async (e) => {
        console.log("DBG thread", JSON.stringify(await bob.get(`/threads/${ctx.thread.id}`)).slice(0, 1500));
        console.log("DBG run", JSON.stringify(await bob.get(`/runs/${run.id}`)).slice(0, 800));
        console.log("DBG metrics", (await (await fetch(env.S.workerAdmin + "/metrics")).text()).split("\n").filter((l) => l.includes("runs_")).join(" "));
        throw e;
      });
      eq(waiting.pendingApproval.toolName, "execute_command", "the tool that waits");
      ctx.approvalId = waiting.pendingApproval.id ?? waiting.pendingApproval.approvalId;
      // alice sees it in her inbox (HTTP) and in her browser
      const inbox = await poll(async () => {
        const r = await alice.get("/needs-review");
        return JSON.stringify(r).includes(ctx.thread.id) ? r : null;
      }, "alice's needs-review lists bob's session", 20_000);
      ok(inbox, "inbox");
      await alice.page.goto(`${env.S.base}/#/inbox`);
      await env.visible(alice.page.getByRole("main").getByText("Bob needs approval"), "bob's session in alice's Needs you");
      // dave (viewer) must not be able to answer it
      const denied = await dave.api("POST", `/threads/${ctx.thread.id}/resume`, { interruptId: ctx.approvalId, reason: "tool_approval", decision: "approve" });
      ok([403, 404].includes(denied.status), `a viewer answering the approval -> ${denied.status} ${denied.text}`);
      // the run sits on the worker: its runtime id is the worker's
      const r = await bob.get(`/runs/${ctx.run.id}`);
      ctx.runtime = r.runtimeId ?? r.runtime_id ?? r.owner ?? null;
    },
  ],
  [
    "alice approves it from the session card; bob's run continues and finishes",
    async (env) => {
      const alice = await env.person("alice");
      const bob = await env.person("bob");
      await alice.page.getByRole("main").getByText("Bob needs approval").first().click();
      const card = alice.page.getByRole("alertdialog", { name: "Approval needed" });
      await env.visible(card, "the approval card in alice's browser");
      await env.visible(card.getByText("execute_command"), "the tool");
      await card.getByRole("button", { name: "Approve", exact: true }).click();
      await env.visible(alice.page.getByText("The script finished."), "the final answer after the approval");
      await env.idle(bob, ctx.thread.id);
      const t = await bob.get(`/threads/${ctx.thread.id}`);
      eq(t.pendingApproval ?? null, null, "nothing pending");
      eq(t.messages.at(-1).role, "assistant", "the run ended with an answer");
      const r = await bob.get(`/runs/${ctx.run.id}`);
      ok(!r.error, `run record: ${JSON.stringify(r).slice(0, 300)}`);
    },
  ],
  [
    "the audit log shows the request and alice's decision",
    async (env) => {
      const alice = await env.person("alice");
      const audit = await poll(async () => {
        const a = await alice.get(`/projects/${ctx.project.id}/audit`);
        const text = JSON.stringify(a);
        return /approv/i.test(text) ? a : null;
      }, "the audit log mentions the approval", 20_000);
      const text = JSON.stringify(audit);
      ok(text.includes("bob") && text.includes("alice"), `audit names both people: ${text.slice(0, 600)}`);
    },
  ],
  [
    "approval latency appears in Insights People for alice",
    async (env) => {
      const alice = await env.person("alice");
      const ins = await poll(async () => {
        const i = await alice.get(`/projects/${ctx.project.id}/insights`);
        return /approv/i.test(JSON.stringify(i.people ?? i)) ? i : null;
      }, "insights has approval figures", 20_000);
      const people = JSON.stringify(ins.people ?? ins);
      ok(people.includes("alice"), `alice is among the people: ${people.slice(0, 600)}`);
    },
  ],
];
