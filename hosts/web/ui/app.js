"use strict";
// Harness web UI: talks to the JSON API under /api and follows a run's AG-UI events over SSE.
const $ = (id) => document.getElementById(id);
const narrow = () => matchMedia("(max-width: 800px)").matches;
const S = { threads: [], tid: null, rid: null, seq: 0, running: false, steps: 0, usage: null, tools: {}, text: null, wsPath: "" };

function el(tag, cls, ...kids) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  for (const k of kids) e.append(k);
  return e;
}

// ---- API ----
function token() { try { return sessionStorage.getItem("harness-token") || ""; } catch (_) { return ""; } }
function headers(extra) {
  const h = Object.assign({ "Content-Type": "application/json" }, extra);
  if (token()) h.Authorization = "Bearer " + token();
  return h;
}
async function raw(path, opts) {
  let r = await fetch("/api" + path, Object.assign({}, opts, { headers: headers(opts && opts.headers) }));
  if (r.status === 401) {
    const t = prompt("Access token (HARNESS_WEB_TOKEN)");
    if (!t) throw new Error("not authenticated");
    try { sessionStorage.setItem("harness-token", t); } catch (_) { /* private mode */ }
    r = await fetch("/api" + path, Object.assign({}, opts, { headers: headers(opts && opts.headers) }));
  }
  return r;
}
async function api(path, method, body) {
  const r = await raw(path, { method: method || "GET", body: body === undefined ? undefined : JSON.stringify(body) });
  if (r.status === 204) return null;
  const j = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error((j.error && j.error.message) || r.statusText);
  return j;
}
function fail(e) { note(String(e.message || e), "error"); }

// ---- markdown (DOM only, never innerHTML) ----
function inline(parent, text) {
  const re = /(`[^`]+`|\*\*[^*]+\*\*)/g;
  text.split(re).forEach((part) => {
    if (part.startsWith("`") && part.length > 2) parent.append(el("code", "", part.slice(1, -1)));
    else if (part.startsWith("**") && part.length > 4) parent.append(el("strong", "", part.slice(2, -2)));
    else parent.append(part);
  });
}
function markdown(src) {
  const root = document.createDocumentFragment();
  let list = null, para = null, code = null;
  const flush = () => { list = null; para = null; };
  for (const line of src.split("\n")) {
    if (code) {
      if (line.startsWith("```")) { code = null; } else { code.textContent += (code.textContent ? "\n" : "") + line; }
      continue;
    }
    if (line.startsWith("```")) { flush(); const c = el("code"); root.append(el("pre", "", c)); code = c; continue; }
    const m = /^\s*([-*]|\d+\.)\s+(.*)/.exec(line);
    if (m) {
      const kind = /\d/.test(m[1]) ? "ol" : "ul";
      if (!list || list.tagName.toLowerCase() !== kind) { para = null; list = el(kind); root.append(list); }
      const li = el("li"); inline(li, m[2]); list.append(li);
    } else if (!line.trim()) { flush(); }
    else {
      list = null;
      if (!para) { para = el("p"); root.append(para); } else para.append(document.createElement("br"));
      inline(para, line);
    }
  }
  return root;
}
function renderText(box, text) { box.replaceChildren(markdown(text)); }

// ---- transcript ----
const tr = () => $("transcript");
function scroll() { const t = tr(); if (t.scrollHeight - t.scrollTop - t.clientHeight < 200) t.scrollTop = t.scrollHeight; }
function note(text, cls) { tr().append(el("div", "msg note " + (cls || ""), text)); scroll(); }
function userMsg(text) { tr().append(el("div", "msg user", text)); scroll(); }
function assistantBox(text) { const b = el("div", "msg"); b.dataset.raw = text || ""; renderText(b, b.dataset.raw); tr().append(b); return b; }
function parse(args) { try { return JSON.parse(args) || {}; } catch (_) { return {}; } }
function summary(a) {
  for (const k of ["path", "command", "cmd", "pattern", "query", "url", "name", "question"]) if (typeof a[k] === "string" && a[k]) return a[k].replace(/\s+/g, " ").slice(0, 120);
  return "";
}
function diffOf(a) {
  if (typeof a.oldText !== "string" || typeof a.newText !== "string") return null;
  const pre = el("pre", "diff");
  a.oldText.split("\n").forEach((l) => pre.append(el("span", "del", "- " + l)));
  a.newText.split("\n").forEach((l) => pre.append(el("span", "add", "+ " + l)));
  return pre;
}
function toolBlock(id, name, args) {
  const a = parse(args);
  const d = el("details", "tool", el("summary", "", name + " " + summary(a)));
  const out = el("pre");
  const diff = name === "edit_file" ? diffOf(a) : null;
  if (diff) d.append(diff); else if (args && args !== "{}") d.append(el("pre", "", args.slice(0, 2000)));
  d.append(out);
  tr().append(d);
  S.tools[id] = { d, out, name, diff };
  scroll();
}
function toolResult(id, content, isErr) {
  const t = S.tools[id];
  if (!t) return;
  if (isErr) t.d.classList.add("err");
  const clip = content.length > 4000 ? content.slice(0, 4000) + "\n… (" + (content.length - 4000) + " more characters)" : content;
  t.out.textContent = clip;
  if (t.diff && !isErr) t.out.classList.add("hidden");
}

function renderMessages(msgs) {
  tr().replaceChildren();
  S.tools = {};
  for (const m of msgs) {
    if (m.role === "user") { if (m.metadata && m.metadata.steer) note("steer: " + m.content); else userMsg(m.content); }
    else if (m.role === "assistant") {
      if (m.content) assistantBox(m.content);
      for (const c of m.toolCalls || []) toolBlock(c.id, c.function.name, c.function.arguments);
      if (m.error) note(m.error, "error");
    } else if (m.role === "tool") toolResult(m.toolCallId, m.content, !!m.error);
  }
  tr().scrollTop = tr().scrollHeight;
}

// ---- status ----
function status() {
  const u = S.usage, parts = [$("model").value || "no model"];
  parts.push(S.running ? "running" : "idle", S.steps + " steps");
  if (u) {
    const inT = u.inputTokens || 0;
    parts.push((inT / 1000).toFixed(1) + "k in", ((u.outputTokens || 0) / 1000).toFixed(1) + "k out",
      (inT ? Math.round(((u.cachedInputTokens || 0) * 100) / inT) : 0) + "% cached");
  }
  $("status").textContent = parts.join(" · ");
  $("cancel").classList.toggle("hidden", !S.running);
  $("send").textContent = S.running ? "Steer" : "Send";
}

// ---- events ----
function onEvent(ev) {
  switch (ev.type) {
    case "STEP_STARTED": S.steps++; break;
    case "TEXT_MESSAGE_START": S.text = assistantBox(""); break;
    case "TEXT_MESSAGE_CONTENT":
      if (!S.text) S.text = assistantBox("");
      S.text.dataset.raw += ev.delta || "";
      renderText(S.text, S.text.dataset.raw);
      scroll();
      break;
    case "TEXT_MESSAGE_END": S.text = null; break;
    case "TOOL_CALL_START": toolBlock(ev.toolCallId, ev.toolCallName, ""); S.tools[ev.toolCallId].args = ""; break;
    case "TOOL_CALL_ARGS": { const t = S.tools[ev.toolCallId]; if (t) { t.args += ev.delta || ""; t.d.firstChild.textContent = t.name + " " + summary(parse(t.args)); } break; }
    case "TOOL_CALL_END": {
      const t = S.tools[ev.toolCallId];
      if (t && t.name === "edit_file" && !t.diff) { const d = diffOf(parse(t.args)); if (d) { t.diff = d; t.d.insertBefore(d, t.out); } }
      break;
    }
    case "TOOL_CALL_RESULT": toolResult(ev.toolCallId, ev.content || "", !!(ev.metadata && ev.metadata.isError)); break;
    case "CUSTOM":
      if (ev.name === "work.usage") S.usage = ev.value;
      else if (ev.name === "work.tool_output" && S.tools[ev.value.toolCallId]) S.tools[ev.value.toolCallId].out.textContent += ev.value.text || "";
      else if (ev.name === "work.steer" && ev.value && ev.value.delivered === false) note("(your message arrives with the next run)");
      break;
    case "RUN_ERROR": note((ev.code ? ev.code + ": " : "") + (ev.message || "run failed"), "error"); break;
    case "RUN_FINISHED": {
      const o = ev.outcome || {};
      if (ev.usage && ev.usage.length) S.usage = S.usage || ev.usage[0];
      if (o.type === "interrupt" && o.interrupts && o.interrupts.length) askUser(o.interrupts[0]);
      else if (o.type === "cancelled") note("cancelled");
      break;
    }
  }
}

async function follow(runId, after) {
  S.rid = runId; S.seq = after || 0; S.running = true; S.steps = S.steps || 0;
  status();
  let finished = false, tries = 0;
  while (!finished && S.rid === runId && tries < 6) {
    try {
      const r = await raw("/runs/" + runId + "/events?after=" + S.seq, { headers: { Accept: "text/event-stream", "Last-Event-ID": String(S.seq) } });
      if (!r.ok) throw new Error("events: " + r.statusText);
      const rd = r.body.getReader(), dec = new TextDecoder();
      let buf = "";
      tries = 0;
      for (;;) {
        const { done, value } = await rd.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        let i;
        while ((i = buf.indexOf("\n\n")) >= 0) {
          const chunk = buf.slice(0, i); buf = buf.slice(i + 2);
          let id = null, data = "";
          for (const l of chunk.split("\n")) { if (l.startsWith("id: ")) id = Number(l.slice(4)); else if (l.startsWith("data: ")) data += l.slice(6); }
          if (!data) continue;
          if (id) S.seq = id;
          const ev = JSON.parse(data);
          onEvent(ev);
          if (ev.type === "RUN_FINISHED" || ev.type === "RUN_ERROR") finished = true;
          status();
        }
      }
    } catch (e) { tries++; await new Promise((ok) => setTimeout(ok, 1000 * tries)); }
  }
  if (S.rid === runId) { S.running = false; status(); loadThreads(); loadFiles(); }
}

// ---- approvals and questions ----
function clearPrompt() { $("prompt").classList.add("hidden"); $("prompt").replaceChildren(); }
function askUser(i) {
  const p = $("prompt"), row = el("div", "row");
  p.replaceChildren(); p.classList.remove("hidden");
  const resume = async (body) => {
    clearPrompt();
    try { const run = await api("/threads/" + S.tid + "/resume", "POST", Object.assign({ model: $("model").value, interactive: $("ask").checked }, body)); follow(run.id, 0); }
    catch (e) { fail(e); }
  };
  if (i.reason === "tool_approval") {
    const a = i.approval || {};
    p.append(el("strong", "", "Approval needed: " + (a.toolName || "tool")), el("pre", "", a.toolInputPreview || i.message || ""));
    const id = a.id || i.id, mk = (label, cls, body) => { const b = el("button", cls, label); b.onclick = () => resume(Object.assign({ interruptId: id, reason: "tool_approval" }, body)); row.append(b); };
    mk("Approve once", "primary", { decision: "approve" });
    mk("Approve for this run", "", { decision: "approve", always: true });
    mk("Deny", "danger", { decision: "deny" });
  } else {
    p.append(el("strong", "", i.message || "The agent has a question"));
    const send = (t) => resume({ interruptId: i.id, reason: i.reason, text: t });
    const schema = i.responseSchema || {};
    for (const o of schema.options || []) { const b = el("button", "", o.label); b.onclick = () => send(o.label); row.append(b); }
    if (schema.allowFreeText !== false) {
      const t = el("input"); t.placeholder = "Your answer";
      t.onkeydown = (e) => { if (e.key === "Enter" && t.value.trim()) send(t.value.trim()); };
      const b = el("button", "primary", "Answer"); b.onclick = () => t.value.trim() && send(t.value.trim());
      row.append(t, b);
    }
  }
  p.append(row);
}
function pendingFromThread(t) {
  if (t.pendingApproval) askUser({ reason: "tool_approval", approval: t.pendingApproval, id: t.pendingApproval.id });
  else if (t.pendingQuestion) {
    const q = t.pendingQuestion;
    askUser({ id: q.interruptId, reason: "ask_user", message: q.question, responseSchema: { options: q.options, allowFreeText: q.allowFreeText } });
  }
}

// ---- threads ----
async function loadThreads() {
  try { S.threads = await api("/threads?limit=100"); } catch (e) { return fail(e); }
  const ul = $("thread-list");
  ul.replaceChildren();
  for (const t of S.threads) {
    const del = el("button", "", "×"); del.title = "Delete";
    del.onclick = async (e) => { e.stopPropagation(); if (confirm("Delete this conversation?")) { await api("/threads/" + t.id, "DELETE").catch(fail); if (S.tid === t.id) newThread(); loadThreads(); } };
    const name = el("span", "", t.title || "(untitled)");
    name.ondblclick = async () => { const n = prompt("Rename", t.title); if (n && n.trim()) { await api("/threads/" + t.id, "PATCH", { title: n.trim() }).catch(fail); loadThreads(); } };
    const li = el("li", t.id === S.tid ? "sel" : "", name, del);
    li.onclick = () => openThread(t.id);
    ul.append(li);
  }
}
function newThread() {
  S.tid = null; S.rid = null; S.running = false; S.steps = 0; S.usage = null;
  tr().replaceChildren(); clearPrompt(); $("title").textContent = "New conversation"; $("file-list").replaceChildren();
  status(); loadThreads();
  if (narrow()) $("threads").classList.add("hidden");
}
async function openThread(id) {
  S.rid = null; S.running = false; S.steps = 0; S.usage = null; clearPrompt();
  let t;
  try { t = await api("/threads/" + id); } catch (e) { return fail(e); }
  S.tid = id; $("title").textContent = t.title || "(untitled)";
  renderMessages(t.messages);
  if (t.activeRun) follow(t.activeRun.id, 0); else pendingFromThread(t);
  status(); loadThreads(); loadFiles("");
  if (narrow()) $("threads").classList.add("hidden");
}

// ---- send / steer / cancel ----
async function submit(e) {
  e.preventDefault();
  const text = $("input").value.trim();
  if (!text) return;
  $("input").value = "";
  try {
    if (S.running && S.rid) { await api("/runs/" + S.rid + "/steer", "POST", { text }); note("steer: " + text); return; }
    if (!S.tid) { const t = await api("/threads", "POST", {}); S.tid = t.id; }
    clearPrompt(); userMsg(text);
    const run = await api("/threads/" + S.tid + "/runs", "POST", { message: text, model: $("model").value, interactive: $("ask").checked });
    follow(run.id, 0);
    loadThreads();
  } catch (err) { fail(err); }
}

// ---- files ----
async function loadFiles(p) {
  if (p !== undefined) S.wsPath = p;
  const ul = $("file-list");
  if (!S.tid) return ul.replaceChildren();
  let files;
  try { files = await api("/threads/" + S.tid + "/files?path=" + encodeURIComponent(S.wsPath)); } catch (e) { ul.replaceChildren(el("li", "muted", String(e.message))); return; }
  $("crumbs").textContent = "/workspace" + (S.wsPath ? "/" + S.wsPath.replace(/^\/workspace\/?/, "") : "");
  ul.replaceChildren();
  if (S.wsPath && S.wsPath !== "/workspace") { const up = el("li", "", el("span", "", "..")); up.onclick = () => loadFiles(S.wsPath.replace(/\/[^/]*$/, "") || ""); ul.append(up); }
  for (const f of files) {
    if (f.type === "directory") { const li = el("li", "", el("span", "", f.name + "/")); li.onclick = () => loadFiles(f.path); ul.append(li); continue; }
    const a = el("a", "", f.name); a.href = "#"; a.title = f.size + " bytes";
    a.onclick = (e) => { e.preventDefault(); download(f); };
    ul.append(el("li", "", a, el("span", "muted", Math.ceil(f.size / 1024) + " KB")));
  }
  if (!files.length) ul.append(el("li", "muted", "empty"));
}
async function download(f) {
  try {
    const r = await raw("/threads/" + S.tid + "/files/download?path=" + encodeURIComponent(f.path));
    if (!r.ok) throw new Error("download failed: " + r.statusText);
    const url = URL.createObjectURL(await r.blob());
    const a = el("a"); a.href = url; a.download = f.name; document.body.append(a); a.click(); a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 10000);
  } catch (e) { fail(e); }
}

// ---- init ----
async function init() {
  $("new").onclick = newThread;
  $("composer").onsubmit = submit;
  $("input").onkeydown = (e) => { if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); $("composer").requestSubmit(); } };
  $("cancel").onclick = () => S.rid && api("/runs/" + S.rid + "/cancel", "POST").catch(fail);
  $("toggle-threads").onclick = () => $("threads").classList.toggle("hidden");
  $("toggle-files").onclick = () => { $("files").classList.toggle("hidden"); loadFiles(); };
  $("refresh-files").onclick = () => loadFiles();
  $("model").onchange = status;
  if (narrow()) $("threads").classList.add("hidden");
  try {
    const m = await api("/models");
    for (const id of m.models) $("model").append(new Option(id, id));
    if (m.default) $("model").value = m.default;
  } catch (e) { fail(e); }
  status();
  loadThreads();
}
init();
