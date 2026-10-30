import { describe, expect, it } from "vitest";
import { blobToDataUrl, cn } from "./utils";

describe("cn", () => {
  it("drops falsy values and lets later Tailwind classes win", () => {
    expect(cn("p-2", false && "x", undefined, "p-4", ["font-bold"])).toBe("p-4 font-bold");
  });
});

describe("blobToDataUrl", () => {
  it("encodes the blob as a data: URL with its type", async () => {
    expect(await blobToDataUrl(new Blob(["hi"], { type: "text/plain" }))).toBe("data:text/plain;base64,aGk=");
  });
});
