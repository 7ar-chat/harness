import { describe, expect, it } from "vitest";
import { formatUsage } from "./usage";

describe("formatUsage", () => {
  it("is empty without usage", () => {
    expect(formatUsage(undefined)).toBe("");
  });
  it("abbreviates thousands and omits what is unknown", () => {
    expect(formatUsage({ inputTokens: 1234, outputTokens: 56 })).toBe("1.2k in · 56 out");
    expect(formatUsage({})).toBe("0 in · 0 out");
  });
  it("shows the cached share of the input and the context fill", () => {
    expect(formatUsage({ inputTokens: 1000, outputTokens: 10, cachedInputTokens: 250, contextTokens: 50, contextWindow: 200 })).toBe("1.0k in · 10 out · 25% cached · ctx 25%");
  });
  it("prefers the reported cost and keeps three decimals below ten cents", () => {
    expect(formatUsage({ inputTokens: 1, outputTokens: 1, costUsd: 0.0123 })).toBe("1 in · 1 out · $0.012");
    expect(formatUsage({ inputTokens: 1, outputTokens: 1, costUsd: 1.5 })).toBe("1 in · 1 out · $1.50");
  });
  it("estimates the cost from the model price when none is reported, only for priced models", () => {
    const u = { inputTokens: 1000, outputTokens: 100 };
    expect(formatUsage(u, { priced: true, inputPerToken: 0.00001, outputPerToken: 0.00003 })).toBe("1.0k in · 100 out · $0.013");
    expect(formatUsage(u, { priced: false, inputPerToken: 0.00001, outputPerToken: 0.00003 })).toBe("1.0k in · 100 out");
    expect(formatUsage(u, { priced: true })).toBe("1.0k in · 100 out");
  });
});
