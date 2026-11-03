import { describe, expect, it } from "vitest";
import { avatarArt, hashString } from "./avatar";

const HSL = /^hsl\(\d{1,3} \d{1,3}% \d{1,3}%\)$/;

describe("avatarArt", () => {
  it("is deterministic", () => {
    expect(avatarArt("alice")).toEqual(avatarArt("alice"));
    expect(hashString("alice")).toBe(hashString("alice"));
  });

  it("differs between logins", () => {
    const seen = new Set(["alice", "bob", "github_carol", "dave", "erin"].map((l) => JSON.stringify(avatarArt(l))));
    expect(seen.size).toBe(5);
  });

  it("only produces valid colors and shapes inside the canvas", () => {
    for (const l of ["a", "alice", "github_x", "", "ünïcode", "x".repeat(200)]) {
      const art = avatarArt(l);
      expect(art.from).toMatch(HSL);
      expect(art.to).toMatch(HSL);
      expect(art.blobs).toHaveLength(3);
      for (const b of art.blobs) {
        expect(b.color).toMatch(HSL);
        expect(b.cx).toBeGreaterThanOrEqual(0);
        expect(b.cx).toBeLessThanOrEqual(100);
        expect(b.cy).toBeGreaterThanOrEqual(0);
        expect(b.cy).toBeLessThanOrEqual(100);
        expect(b.r).toBeGreaterThan(0);
        expect(b.opacity).toBeGreaterThan(0);
        expect(b.opacity).toBeLessThan(1);
      }
    }
  });
});
