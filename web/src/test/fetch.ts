// A fake `fetch` for the API: routes are matched on "METHOD /api/path" (query string included).
import { vi } from "vitest";

export interface Call {
  method: string;
  url: string;
  headers: Record<string, string>;
  body: unknown;
}
export type Handler = unknown | ((c: Call) => unknown);
export type Routes = Record<string, Handler>;

/** A response that is JSON by default; pass `{ status, body }` through `reply` for anything else. */
export const reply = (status: number, body?: unknown, init: ResponseInit = {}) =>
  new Response(body === undefined ? null : typeof body === "string" ? body : JSON.stringify(body), { status, ...init });

/** Stream that never ends: stands in for the live /api/events feed. */
export const hangingStream = () => new Response(new ReadableStream<Uint8Array>({ start() {} }), { status: 200, headers: { "Content-Type": "text/event-stream" } });

export function mockFetch(routes: Routes) {
  const calls: Call[] = [];
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === "string" ? input : input instanceof URL ? input.pathname + input.search : input.url;
    const method = (init?.method ?? "GET").toUpperCase();
    const headers = Object.fromEntries(Object.entries((init?.headers ?? {}) as Record<string, string>));
    let body: unknown = init?.body;
    if (typeof body === "string") {
      try {
        body = JSON.parse(body);
      } catch {
        /* keep the raw text */
      }
    }
    const call: Call = { method, url, headers, body };
    calls.push(call);
    const h = routes[`${method} ${url}`] ?? routes[`${method} ${url.split("?")[0]}`];
    if (h === undefined) {
      if (url === "/api/events") return hangingStream();
      return reply(404, { error: { code: "not_found", message: `no mock for ${method} ${url}` } });
    }
    const v = typeof h === "function" ? (h as (c: Call) => unknown)(call) : h;
    return v instanceof Response ? v : reply(200, v);
  });
  vi.stubGlobal("fetch", fn);
  return { fn, calls, find: (method: string, prefix: string) => calls.filter((c) => c.method === method && c.url.startsWith(prefix)) };
}
