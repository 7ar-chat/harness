import { beforeEach, describe, expect, it, vi } from "vitest";
import { openSse, parseSse } from "./sse";

describe("parseSse", () => {
  it("parses ids, events and multi-line data, keeping a partial block", () => {
    const r = parseSse(": connected\n\nid: 3\ndata: {\"a\":1}\n\nevent: run.started\ndata: x\ndata: y\n\nid: 4\ndata: par");
    expect(r.messages).toEqual([
      { id: "3", event: "message", data: '{"a":1}' },
      { id: undefined, event: "run.started", data: "x\ny" },
    ]);
    expect(r.rest).toBe("id: 4\ndata: par");
  });
  it("accepts CRLF", () => {
    expect(parseSse("id: 1\r\ndata: z\r\n\r\n").messages[0]).toMatchObject({ id: "1", data: "z" });
  });
});

describe("parseSse edge cases", () => {
  it("handles field-only lines, a missing space after the colon and unknown fields", () => {
    const r = parseSse("data\ndata:no-space\ndata: keep  two\nretry: 5\nbogus: 1\n\n");
    expect(r.messages).toEqual([{ id: undefined, event: "message", data: "\nno-space\nkeep  two" }]);
  });
  it("keeps an id-only block (heartbeat that moves the cursor) but drops comment-only ones", () => {
    const r = parseSse(": ping\n\nid: 9\n\n");
    expect(r.messages).toEqual([{ id: "9", event: "message", data: "" }]);
  });
  it("returns everything as the rest when no block is complete", () => {
    expect(parseSse("data: x\n")).toEqual({ messages: [], rest: "data: x\n" });
  });
});

const enc = new TextEncoder();
/** A response whose body yields the given chunks, then ends (or errors with `fail`). */
const streamOf = (chunks: string[], fail?: Error) =>
  new Response(
    new ReadableStream<Uint8Array>({
      start(c) {
        for (const x of chunks) c.enqueue(enc.encode(x));
        if (fail) c.error(fail);
        else c.close();
      },
    }),
    { status: 200 },
  );

describe("openSse", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  it("delivers events split across chunks and sends Accept, credentials and Last-Event-ID", async () => {
    // Arrange
    const fetchMock = vi.fn(async () => streamOf(['id: 1\ndata: {"a"', ":1}\n\nid: 2\nda", "ta: b\n\n"]));
    vi.stubGlobal("fetch", fetchMock);
    const got: unknown[] = [];
    const gaveUp = vi.fn();
    // Act
    openSse({ url: "/api/x", lastEventId: "0", stopOnClose: true, onMessage: (m) => got.push(m), onGiveUp: gaveUp });
    await vi.waitFor(() => expect(gaveUp).toHaveBeenCalled());
    // Assert
    expect(got).toEqual([
      { id: "1", event: "message", data: '{"a":1}' },
      { id: "2", event: "message", data: "b" },
    ]);
    expect(fetchMock).toHaveBeenCalledWith("/api/x", expect.objectContaining({ credentials: "same-origin", headers: expect.objectContaining({ Accept: "text/event-stream", "Last-Event-ID": "0" }) }));
  });

  it("reconnects after a clean close, resuming from the last seen id, and reports the reconnect", async () => {
    const urls: string[] = [];
    const lastIds: (string | undefined)[] = [];
    let n = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (u: string, init: RequestInit) => {
        urls.push(u);
        lastIds.push((init.headers as Record<string, string>)["Last-Event-ID"]);
        return n++ === 0 ? streamOf(["id: 5\ndata: a\n\n"]) : streamOf(["id: 6\ndata: b\n\n"]);
      }),
    );
    const opens: boolean[] = [];
    const got: string[] = [];
    let url = 0;
    const close = openSse({ url: () => `/api/x?n=${url++}`, onOpen: (r) => opens.push(r), onMessage: (m) => got.push(m.data) });
    await vi.waitFor(() => expect(got).toEqual(["a"]));
    await vi.advanceTimersByTimeAsync(1000); // a clean close reconnects after the first backoff step
    await vi.waitFor(() => expect(got).toEqual(["a", "b"]));
    close();
    expect(opens).toEqual([false, true]);
    expect(lastIds).toEqual([undefined, "5"]);
    expect(urls).toEqual(["/api/x?n=0", "/api/x?n=1"]);
  });

  it("backs off exponentially on failures and gives up after maxRetries", async () => {
    const fetchMock = vi.fn(async () => new Response("no", { status: 500 }));
    vi.stubGlobal("fetch", fetchMock);
    const errors: unknown[] = [];
    const gaveUp = vi.fn();
    openSse({ url: "/api/x", maxRetries: 2, onMessage: () => {}, onError: (e) => errors.push(e), onGiveUp: gaveUp });
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    await vi.advanceTimersByTimeAsync(999); // first backoff is 1000ms
    expect(fetchMock).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(2000); // second is 2000ms
    expect(fetchMock).toHaveBeenCalledTimes(3);
    await vi.waitFor(() => expect(gaveUp).toHaveBeenCalledTimes(1));
    expect(errors).toHaveLength(3);
    expect(errors[0]).toMatchObject({ message: "stream 500" });
    await vi.advanceTimersByTimeAsync(60_000);
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it("a successful connection resets the retry budget", async () => {
    let n = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => (++n === 2 ? streamOf([], new Error("reset")) : new Response("no", { status: 503 }))),
    );
    const gaveUp = vi.fn();
    const opens = vi.fn();
    openSse({ url: "/api/x", maxRetries: 1, onMessage: () => {}, onOpen: opens, onGiveUp: gaveUp });
    await vi.waitFor(() => expect(n).toBe(1));
    await vi.advanceTimersByTimeAsync(1000);
    await vi.waitFor(() => expect(opens).toHaveBeenCalledTimes(1));
    expect(gaveUp).not.toHaveBeenCalled(); // the stream error after a successful open still has a retry left
    await vi.advanceTimersByTimeAsync(1000);
    await vi.waitFor(() => expect(gaveUp).toHaveBeenCalledTimes(1));
    expect(n).toBe(3);
  });

  it("on 401 signals the session is gone and stops without retrying", async () => {
    const fetchMock = vi.fn(async () => new Response(null, { status: 401 }));
    vi.stubGlobal("fetch", fetchMock);
    const unauthorized = vi.fn();
    window.addEventListener("harness:unauthorized", unauthorized);
    openSse({ url: "/api/x", onMessage: () => {} });
    await vi.waitFor(() => expect(unauthorized).toHaveBeenCalledTimes(1));
    await vi.advanceTimersByTimeAsync(60_000);
    window.removeEventListener("harness:unauthorized", unauthorized);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("close() aborts the request and stays quiet", async () => {
    let signal: AbortSignal | undefined;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (_u: string, init: RequestInit) => {
        signal = init.signal ?? undefined;
        return new Promise<Response>((_, rej) => signal?.addEventListener("abort", () => rej(new DOMException("aborted", "AbortError"))));
      }),
    );
    const onError = vi.fn();
    const close = openSse({ url: "/api/x", onMessage: () => {}, onError });
    await vi.waitFor(() => expect(signal).toBeDefined());
    close();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(signal?.aborted).toBe(true);
    expect(onError).not.toHaveBeenCalled();
  });
});
