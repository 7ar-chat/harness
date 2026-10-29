import { describe, expect, it } from "vitest";
import { parseSse } from "./sse";

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
