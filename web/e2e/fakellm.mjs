// A scripted, OpenAI-compatible fake LLM for the browser e2e: no network, deterministic answers.
//
//   POST /v1/chat/completions   stream:true -> SSE chat.completion.chunk frames + [DONE]; otherwise one JSON body
//   GET  /v1/models             {"data":[{"id":"gpt-test"}]}
//   POST /__release             lets a held reply continue (see "hold" below)
//
// What it answers is decided from the request alone (last message, tools offered, user text):
//   * no tools offered (title generation, summaries, ...)     -> "A title"
//   * last message is a tool result                           -> a closing text for that tool (see closing())
//   * user text contains "write a file"                       -> tool call write_file{report.md}
//   * user text contains "run the risky script"               -> tool call execute_command `node -e "require('fs').writeFileSync('denied.txt','ran')"`
//   * user text contains "run the script"                     -> tool call execute_command `node -e "require('fs').writeFileSync('ran.txt','ran')"`
//   * anything else                                           -> a plain streamed text
//
// The closing text after a write_file is streamed in pieces and HELD before its last piece until the test calls
// release(): the test can then assert the partial text is on screen while the run is still going.
import http from "node:http";

const delay = (ms) => new Promise((r) => setTimeout(r, ms));

const textOf = (content) =>
  typeof content === "string" ? content : Array.isArray(content) ? content.map((p) => (typeof p === "string" ? p : p?.text ?? "")).join("") : "";

/** The scripted reply of one request: { text: string[] | null, hold: boolean, call: {name, args} | null }. */
export function script(body) {
  const msgs = body.messages ?? [];
  const last = msgs[msgs.length - 1] ?? {};
  if (!body.tools?.length) return { text: ["A title"], call: null };
  if (last.role === "tool") return closing(msgs, textOf(last.content));
  const user = textOf([...msgs].reverse().find((m) => m.role === "user")?.content);
  if (user.includes("write a file"))
    return { call: { name: "write_file", args: { path: "report.md", content: "# Report v1\n" } } };
  if (user.includes("run the risky script")) return { call: { name: "execute_command", args: { command: `node -e "require('fs').writeFileSync('denied.txt','ran')"` } } };
  if (user.includes("run the script")) return { call: { name: "execute_command", args: { command: `node -e "require('fs').writeFileSync('ran.txt','ran')"` } } };
  return { text: ["Hello ", "from ", "the ", "fake ", "model."] };
}

/** The text after a tool result. A denied command comes back as an error result; the model then gives up gracefully. */
function closing(msgs, result) {
  const call = [...msgs].reverse().find((m) => m.role === "assistant" && m.tool_calls?.length)?.tool_calls[0];
  const name = call?.function?.name;
  if (name === "write_file") return { text: ["I wrote ", "report.md", " and nothing else", "."], hold: true };
  if (name === "execute_command") {
    const denied = /den(y|ied)|reject|not approved|refus/i.test(result) && !/ran-ok/.test(result);
    return { text: denied ? ["Understood, ", "I did not run it."] : ["The script ", "finished."] };
  }
  return { text: ["Done."] };
}

export async function startFakeLlm() {
  const requests = [];
  let seq = 0;
  let gate = null; // { release, held } of the reply currently held
  const waiters = [];

  const sse = (res, obj) => res.write(`data: ${JSON.stringify(obj)}\n\n`);
  const chunk = (id, delta, finish = null) => ({
    id, object: "chat.completion.chunk", created: 1, model: "gpt-test",
    choices: [{ index: 0, delta, finish_reason: finish }],
  });

  async function holdHere() {
    let release;
    const released = new Promise((r) => (release = r));
    gate = { release };
    waiters.splice(0).forEach((w) => w());
    await released;
    gate = null;
  }

  async function reply(req, res, body) {
    const s = script(body);
    const id = `chatcmpl-${++seq}`;
    const callId = s.call ? `call_${seq}` : null;
    const argsText = s.call ? JSON.stringify(s.call.args) : "";
    if (!body.stream) {
      const message = s.call
        ? { role: "assistant", content: null, tool_calls: [{ id: callId, type: "function", function: { name: s.call.name, arguments: argsText } }] }
        : { role: "assistant", content: s.text.join("") };
      const out = {
        id, object: "chat.completion", created: 1, model: "gpt-test",
        choices: [{ index: 0, message, finish_reason: s.call ? "tool_calls" : "stop" }],
        usage: { prompt_tokens: 10, completion_tokens: 5, total_tokens: 15 },
      };
      res.writeHead(200, { "Content-Type": "application/json" }).end(JSON.stringify(out));
      return;
    }
    res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", Connection: "keep-alive" });
    sse(res, chunk(id, { role: "assistant", content: "" }));
    if (s.call) {
      sse(res, chunk(id, { tool_calls: [{ index: 0, id: callId, type: "function", function: { name: s.call.name, arguments: "" } }] }));
      const mid = Math.ceil(argsText.length / 2);
      for (const part of [argsText.slice(0, mid), argsText.slice(mid)]) {
        await delay(20);
        sse(res, chunk(id, { tool_calls: [{ index: 0, function: { arguments: part } }] }));
      }
      sse(res, chunk(id, {}, "tool_calls"));
    } else {
      for (let i = 0; i < s.text.length; i++) {
        if (s.hold && i === s.text.length - 1) await holdHere();
        await delay(30);
        sse(res, chunk(id, { content: s.text[i] }));
      }
      sse(res, chunk(id, {}, "stop"));
    }
    sse(res, { id, object: "chat.completion.chunk", created: 1, model: "gpt-test", choices: [], usage: { prompt_tokens: 10, completion_tokens: 5, total_tokens: 15 } });
    res.write("data: [DONE]\n\n");
    res.end();
  }

  const server = http.createServer((req, res) => {
    const path = req.url.split("?")[0];
    if (req.method === "GET" && path === "/v1/models") {
      res.writeHead(200, { "Content-Type": "application/json" }).end(JSON.stringify({ object: "list", data: [{ id: "gpt-test", object: "model" }] }));
    } else if (req.method === "POST" && path === "/__release") {
      gate?.release();
      res.writeHead(204).end();
    } else if (req.method === "POST" && path === "/v1/chat/completions") {
      let raw = "";
      req.on("data", (d) => (raw += d));
      req.on("end", () => {
        let body;
        try {
          body = JSON.parse(raw);
        } catch {
          res.writeHead(400).end("bad json");
          return;
        }
        requests.push(body);
        res.on("close", () => gate?.release()); // a cancelled/failed run must not leave the reply held forever
        reply(req, res, body).catch((e) => {
          console.error("fakellm:", e);
          res.destroy();
        });
      });
    } else {
      res.writeHead(404).end("not found");
    }
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  const { port } = server.address();
  return {
    url: `http://127.0.0.1:${port}/v1`,
    requests,
    /** Resolves once a reply is held before its last piece. */
    held: () => (gate ? Promise.resolve() : new Promise((r) => waiters.push(r))),
    release: () => gate?.release(),
    close: () => new Promise((r) => (server.closeAllConnections?.(), server.close(r))),
  };
}
