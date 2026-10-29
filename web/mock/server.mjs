// A tiny stand-in for the harness API with canned data, for looking at the UI without an LLM.
// Serves the built UI (../hosts/web/ui) with the same CSP as the Go server.  node mock/server.mjs [port]
import http from "node:http";
import fs from "node:fs";
import path from "node:path";

const port = Number(process.argv[2] ?? 8099);
const root = path.resolve(import.meta.dirname, "../../hosts/web/ui");
const CSP = "default-src 'self'; script-src 'self'; style-src 'self'; style-src-attr 'unsafe-inline'; img-src 'self' data: https:; connect-src 'self'";
const now = Date.now();
const iso = (m) => new Date(now - m * 60000).toISOString();

const me = { login: "alice", name: "Alice Doe", displayName: "", defaultName: "Alice Doe", email: "alice@example.com", avatarUrl: "/api/users/alice/avatar?v=1", hasCustomAvatar: true };
let tgConnected = false, tgLinkAt = 0;
const avatarSvg = (c) => `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"><rect width="64" height="64" fill="${c}"/><circle cx="32" cy="26" r="12" fill="#fff" opacity=".85"/><rect x="12" y="42" width="40" height="22" rx="11" fill="#fff" opacity=".85"/></svg>`;
const project = { id: "p1", name: "payments-api", description: "The payments service", repoUrl: "https://github.com/acme/payments-api", defaultBranch: "main", createdBy: "alice", createdAt: iso(9000), updatedAt: iso(5), role: "owner" };
const sum = (id, title, status, extra = {}) => ({ id, title, projectId: "p1", createdBy: "alice", kind: "chat", status, createdAt: iso(60), updatedAt: iso(3), ...extra });
const threads = [
  sum("t1", "Fix flaky refund test", "running", { runId: "r1" }),
  sum("t2", "Add idempotency keys", "waiting_approval", { createdBy: "bob", pendingTool: "bash", runId: "r2" }),
  sum("t3", "Upgrade dependencies", "waiting_input", { pendingQuestion: "Which Go version should I target?" }),
  sum("t4", "Write migration docs", "idle", { updatedAt: iso(300) }),
];
const personal = [{ id: "sv", title: "Global agent", kind: "supervisor", status: "idle", createdBy: "alice", createdAt: iso(100), updatedAt: iso(30) }, { id: "t5", title: "Scratch notes", kind: "chat", status: "idle", createdBy: "alice", createdAt: iso(500), updatedAt: iso(200) }];

const tc = (id, name, args) => ({ id, type: "function", function: { name, arguments: JSON.stringify(args) } });
const messages = [
  { id: "m1", runId: "r0", role: "user", content: "The refund test is flaky on CI. Can you find out why and fix it?", createdAt: iso(50), author: { login: "alice", name: "Alice Doe" } },
  { id: "m2", runId: "r0", role: "assistant", content: "I'll start by reading the test and its helpers.", toolCalls: [tc("c1", "read_file", { path: "/workspace/refund_test.go" })], createdAt: iso(49) },
  { id: "m3", runId: "r0", role: "tool", toolCallId: "c1", content: "package refund\n\nfunc TestRefund(t *testing.T) {\n\ttime.Sleep(10 * time.Millisecond)\n}", createdAt: iso(49) },
  { id: "m4", runId: "r0", role: "assistant", content: "The test sleeps instead of waiting for the worker. I'll replace it with a **channel wait**:\n\n- remove the sleep\n- wait on `done`\n\n```go\n<-done\n```", toolCalls: [tc("c2", "edit_file", { path: "/workspace/refund_test.go", oldText: "\ttime.Sleep(10 * time.Millisecond)\n", newText: "\tselect {\n\tcase <-done:\n\tcase <-time.After(time.Second):\n\t\tt.Fatal(\"timeout\")\n\t}\n" })], createdAt: iso(48) },
  { id: "m5", runId: "r0", role: "tool", toolCallId: "c2", content: '{"path":"refund_test.go","replacements":1}', createdAt: iso(48) },
  { id: "m6", runId: "r0", role: "user", content: "Also check the settlement test please", createdAt: iso(40), author: { login: "bob", name: "Bob Roe" }, metadata: { steer: true } },
  { id: "m7", runId: "r0", role: "user", content: "Run the full suite once more", createdAt: iso(20), author: { kind: "supervisor", onBehalfOf: "carol", login: "" } },
  { id: "m8", runId: "r0", role: "assistant", content: "Done: the suite passes 20/20 runs.", createdAt: iso(19) },
];
const full = (t, extra = {}) => ({ ...t, messages: [], activeRun: null, pendingApproval: null, pendingQuestion: null, ...extra });
const threadDoc = (id) => {
  const t = [...threads, ...personal].find((x) => x.id === id);
  if (!t) return null;
  if (id === "t1") return full(t, { messages, activeRun: { id: "r1", status: "running" }, branch: "fix/flaky-refund", plan: [{ id: "1", title: "Reproduce", status: "completed" }, { id: "2", title: "Replace sleep", status: "in_progress" }, { id: "3", title: "Run the suite", status: "pending" }] });
  if (id === "t2") return full(t, { messages: messages.slice(0, 2), pendingApproval: { id: "ap1", toolName: "bash", toolInputPreview: "go test ./... -count=20" } });
  if (id === "sv") return full(t, { messages: [{ id: "s1", role: "assistant", content: "Hi! Two sessions need you: **Add idempotency keys** (approval) and **Upgrade dependencies** (question).", createdAt: iso(30) }] });
  return full(t, { messages: id === "t4" ? messages.slice(0, 2) : [] });
};

// A recorded AG-UI sequence replayed for run r1.
const ev = (type, f = {}) => ({ type, ...f });
const run1 = [
  ev("RUN_STARTED", { runId: "r1", threadId: "t1" }), ev("STEP_STARTED", { stepName: "model" }),
  ev("REASONING_MESSAGE_START", { messageId: "rs1" }), ev("REASONING_MESSAGE_CONTENT", { messageId: "rs1", delta: "The user wants the suite re-run; check go test flags." }),
  ev("TEXT_MESSAGE_START", { messageId: "a1" }), ev("TEXT_MESSAGE_CONTENT", { messageId: "a1", delta: "Running the tests now, " }), ev("TEXT_MESSAGE_CONTENT", { messageId: "a1", delta: "this can take a minute." }), ev("TEXT_MESSAGE_END", { messageId: "a1" }),
  ev("TOOL_CALL_START", { toolCallId: "x1", toolCallName: "bash" }), ev("TOOL_CALL_ARGS", { toolCallId: "x1", delta: '{"command":"go test ./... -count=20"}' }), ev("TOOL_CALL_END", { toolCallId: "x1" }),
  ev("CUSTOM", { name: "work.tool_output", value: { toolCallId: "x1", text: "ok  \tpayments/refund\t1.2s\n" } }),
  ev("TOOL_CALL_START", { toolCallId: "x2", toolCallName: "task" }), ev("TOOL_CALL_ARGS", { toolCallId: "x2", delta: '{"prompt":"explore"}' }),
  ev("CUSTOM", { name: "work.subagent", value: { toolCallId: "x2", phase: "start", agent: "explore", model: "m" } }),
  ev("CUSTOM", { name: "work.subagent", value: { toolCallId: "x2", phase: "tool", tool: { name: "grep", args: { pattern: "Sleep" } } } }),
  ev("CUSTOM", { name: "work.usage", value: { inputTokens: 12400, outputTokens: 830, cachedInputTokens: 9000 } }),
  ev("STATE_SNAPSHOT", { snapshot: { plan: [{ id: "1", title: "Reproduce", status: "completed" }, { id: "2", title: "Replace sleep", status: "in_progress" }] } }),
];

const hubs = new Set();
const send = (res, status, body) => {
  res.writeHead(status, { "content-type": "application/json", "content-security-policy": CSP });
  res.end(body === undefined ? "" : JSON.stringify(body));
};
const mem = [
  { id: "e1", title: "Use table-driven tests", content: "Prefer table-driven tests with t.Run.", tags: ["go", "testing"], status: "active", updated_at: iso(200), source: "alice" },
  { id: "e2", title: "CI uses Go 1.24", content: "The CI image pins Go 1.24.", tags: ["ci"], status: "pending", updated_at: iso(10), source: "agent" },
];

const server = http.createServer((req, res) => {
  const u = new URL(req.url, "http://x");
  const p = u.pathname;
  if (p.startsWith("/api/")) {
    const a = p.slice(4);
    let um;
    if ((um = /^\/users\/([^/]+)\/avatar$/.exec(a))) { res.writeHead(200, { "content-type": "image/svg+xml", "content-security-policy": CSP }); return res.end(avatarSvg(um[1] === "alice" ? "#6d5dfc" : "#f59e0b")); }
    if (a === "/me/profile" && req.method === "PATCH") { let b = ""; req.on("data", (d) => (b += d)); req.on("end", () => { me.displayName = JSON.parse(b).displayName; me.name = me.displayName || me.defaultName; send(res, 200, me); }); return; }
    if (a === "/me/avatar") { req.resume(); req.on("end", () => { me.hasCustomAvatar = req.method === "PUT"; me.avatarUrl = req.method === "PUT" ? "/api/users/alice/avatar?v=2" : ""; send(res, 200, me); }); return; }
    if (a === "/connectors" && req.method === "GET") {
      if (tgLinkAt && Date.now() - tgLinkAt > 4000) tgConnected = true;
      return send(res, 200, [
        { kind: "github", configured: true, connected: true, account: "alice-dev", label: "GitHub", installUrl: "https://github.com/apps/x" },
        { kind: "telegram", configured: true, connected: tgConnected, account: tgConnected ? "@alice_bot_user" : undefined, label: "Telegram" },
      ]);
    }
    if (a === "/connectors/telegram/link") return send(res, 200, { code: "ABC123", url: "https://t.me/harness_bot?start=ABC123", expiresAt: new Date(Date.now() + 600000).toISOString() }, (tgLinkAt = Date.now()));
    if (a === "/connectors/telegram" && req.method === "DELETE") { tgConnected = false; tgLinkAt = 0; return send(res, 204); }
    if (a === "/connectors/telegram/test") return send(res, 204);
    if (a.startsWith("/connectors/github/repos")) return send(res, 200, { installUrl: "https://github.com/apps/x/installations/new", repos: [
      { fullName: "acme/payments-api", private: true, description: "The payments service", imported: "p1" },
      { fullName: "acme/web-shop", private: false, description: "Storefront in React", defaultBranch: "main" },
      { fullName: "alice-dev/dotfiles", private: true, description: "" } ] });
    if (a === "/projects/import") return send(res, 201, project);
    if (a === "/me") return send(res, 200, { user: me, projects: [project] });
    if (a === "/overview") return send(res, 200, { personal, projects: [{ ...project, threads }] });
    if (a === "/models") return send(res, 200, { models: ["claude-sonnet", "gpt-5"], default: "claude-sonnet" });
    if (a === "/commands") return send(res, 200, [{ name: "review", description: "Review the diff", source: "builtin" }]);
    if (a === "/supervisor") return send(res, 200, threadDoc("sv"));
    if (a === "/supervisor/settings") return send(res, 200, { watch: true, stuckMinutes: 15 });
    if (a === "/projects/p1") return send(res, 200, project);
    if (a === "/projects/p1/members") return send(res, 200, [{ projectId: "p1", login: "alice", name: "Alice Doe", role: "owner", addedAt: iso(900) }, { projectId: "p1", login: "bob", name: "Bob Roe", role: "member", addedAt: iso(800) }]);
    if (a === "/projects/p1/invites") return send(res, 200, [{ projectId: "p1", email: "dave@example.com", role: "viewer", invitedBy: "alice", createdAt: iso(60) }]);
    if (a.startsWith("/projects/p1/audit")) return send(res, 200, [{ id: 1, projectId: "p1", threadId: "t1", actor: "bob", action: "thread.steer", at: iso(40) }]);
    if (a === "/projects/p1/github") return send(res, 200, { configured: true, connected: false, installUrl: "https://github.com/apps/x/installations/new", message: "Install the app on acme/payments-api." });
    if (a === "/memory") return send(res, 200, { entries: mem, enabled: true });
    let m = /^\/threads\/([^/]+)$/.exec(a);
    if (m) { const d = threadDoc(m[1]); return d ? send(res, 200, d) : send(res, 404, { error: { code: "not_found", message: "Not found." } }); }
    if ((m = /^\/threads\/([^/]+)\/files$/.exec(a))) return send(res, 200, u.searchParams.get("path") ? [{ name: "main.go", path: "/workspace/src/main.go", type: "file", size: 120 }] : [{ name: "src", path: "/workspace/src", type: "directory", size: 0 }, { name: "refund_test.go", path: "/workspace/refund_test.go", type: "file", size: 2048 }]);
    if (a === "/events") {
      res.writeHead(200, { "content-type": "text/event-stream", "content-security-policy": CSP });
      res.write(": connected\n\n");
      hubs.add(res); req.on("close", () => hubs.delete(res));
      return;
    }
    if ((m = /^\/runs\/([^/]+)\/events$/.exec(a))) {
      res.writeHead(200, { "content-type": "text/event-stream", "content-security-policy": CSP });
      const after = Number(u.searchParams.get("after") ?? 0);
      run1.forEach((e, i) => i + 1 > after && res.write(`id: ${i + 1}\ndata: ${JSON.stringify(e)}\n\n`));
      req.on("close", () => res.end());
      return; // stays open: the run is still going
    }
    return send(res, 404, { error: { code: "not_found", message: "Not found." } });
  }
  let f = p === "/" ? "/index.html" : p.replace(/^\/ui/, "");
  const file = path.join(root, f);
  if (!file.startsWith(root) || !fs.existsSync(file)) { res.writeHead(404); return res.end(); }
  const types = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css" };
  res.writeHead(200, { "content-type": types[path.extname(file)] ?? "application/octet-stream", "content-security-policy": CSP });
  fs.createReadStream(file).pipe(res);
});
server.listen(port, "127.0.0.1", () => console.log(`mock on http://127.0.0.1:${port}`));
