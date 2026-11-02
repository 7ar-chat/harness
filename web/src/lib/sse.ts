// A small fetch-based SSE client. Unlike EventSource it can send headers (dev
// user switcher), resumes with Last-Event-ID and reconnects with backoff.
import { authHeaders, onUnauthorized } from "@/api/client";

export interface SseMessage {
  id?: string;
  event: string;
  data: string;
}

export interface SseOptions {
  url: string | (() => string);
  /** Called for every event. */
  onMessage: (m: SseMessage) => void;
  /** Called after each (re)connection is established. */
  onOpen?: (reconnect: boolean) => void;
  onError?: (e: unknown) => void;
  /** Initial Last-Event-ID. */
  lastEventId?: string;
  /** Stop reconnecting when this returns true after a stream ends cleanly. */
  stopOnClose?: boolean;
  maxRetries?: number;
  /** Called when the stream ends for good (closed cleanly with stopOnClose, or out of retries). */
  onGiveUp?: () => void;
}

/** Parse "id/event/data" blocks out of a text buffer; returns the rest. */
export function parseSse(buf: string): { messages: SseMessage[]; rest: string } {
  const messages: SseMessage[] = [];
  const norm = buf.replace(/\r\n/g, "\n");
  let rest = norm;
  let i: number;
  while ((i = rest.indexOf("\n\n")) >= 0) {
    const block = rest.slice(0, i);
    rest = rest.slice(i + 2);
    let id: string | undefined;
    let event = "message";
    const data: string[] = [];
    for (const line of block.split("\n")) {
      if (line.startsWith(":") || !line) continue;
      const c = line.indexOf(":");
      const field = c < 0 ? line : line.slice(0, c);
      const value = c < 0 ? "" : line.slice(c + 1).replace(/^ /, "");
      if (field === "id") id = value;
      else if (field === "event") event = value;
      else if (field === "data") data.push(value);
    }
    if (data.length || id !== undefined) messages.push({ id, event, data: data.join("\n") });
  }
  return { messages, rest };
}

/** Starts the stream; returns a function that closes it. */
export function openSse(o: SseOptions): () => void {
  const ctrl = new AbortController();
  let lastId = o.lastEventId;
  let closed = false;

  const run = async () => {
    let attempt = 0;
    let opened = false;
    while (!closed) {
      try {
        const headers: Record<string, string> = { Accept: "text/event-stream", ...authHeaders() };
        if (lastId) headers["Last-Event-ID"] = lastId;
        const url = typeof o.url === "function" ? o.url() : o.url;
        const r = await fetch(url, { headers, signal: ctrl.signal, credentials: "same-origin" });
        if (r.status === 401) {
          onUnauthorized();
          return;
        }
        if (!r.ok || !r.body) throw new Error(`stream ${r.status}`);
        o.onOpen?.(opened);
        opened = true;
        attempt = 0;
        const reader = r.body.getReader();
        const dec = new TextDecoder();
        let buf = "";
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          buf += dec.decode(value, { stream: true });
          const p = parseSse(buf);
          buf = p.rest;
          for (const m of p.messages) {
            if (m.id) lastId = m.id;
            o.onMessage(m);
          }
        }
        if (o.stopOnClose) {
          o.onGiveUp?.();
          return;
        }
      } catch (e) {
        if (closed) return;
        o.onError?.(e);
        if (o.maxRetries !== undefined && attempt >= o.maxRetries) {
          o.onGiveUp?.();
          return;
        }
      }
      attempt++;
      await new Promise((res) => setTimeout(res, Math.min(15000, 500 * 2 ** Math.min(attempt, 5))));
    }
  };
  void run();
  return () => {
    closed = true;
    ctrl.abort();
  };
}
