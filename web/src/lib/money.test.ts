import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { describe, expect, it } from "vitest";
import { formatUsd } from "./money";

describe("formatUsd", () => {
  const cases: [number | null | undefined, string, { compact?: boolean }?][] = [
    [undefined, "n/a"],
    [null, "n/a"],
    [Number.NaN, "n/a"],
    [Infinity, "n/a"],
    [0, "$0"],
    [0.00001, "<$0.0001"],
    [0.0001, "$0.0001"],
    [0.0003, "$0.0003"],
    [0.003, "$0.003"],
    [0.0099, "$0.0099"],
    [0.01, "$0.01"],
    [0.03, "$0.03"],
    [0.0300001, "$0.03"],
    [0.5, "$0.50"],
    [1, "$1.00"],
    [12.5, "$12.50"],
    [999.99, "$999.99"],
    [999.999, "$1,000.00"],
    [1000, "$1,000.00"],
    [1234.5, "$1,234.50"],
    [1234567.891, "$1,234,567.89"],
    [-0.5, "-$0.50"],
    [-0.0003, "-$0.0003"],
    [1234.5, "$1.2k", { compact: true }],
    [1000, "$1k", { compact: true }],
    [3_400_000, "$3.4M", { compact: true }],
    [12.5, "$12.50", { compact: true }],
    [0.0003, "$0.0003", { compact: true }],
  ];
  it.each(cases)("%s -> %s", (n, want, opts) => {
    expect(formatUsd(n, opts)).toBe(want);
  });
  it("never uses scientific notation", () => {
    for (const n of [1e-7, 5e-5, 1e9, 1e21, 123456789012]) expect(formatUsd(n)).not.toMatch(/\de[+-]?\d/i);
  });
});

// Money is formatted by formatUsd and nothing else: a `$` next to toFixed( anywhere else is a second, different format waiting to happen.
describe("money formatting stays in one helper", () => {
  const root = join(__dirname, "..");
  const walk = (d: string): string[] =>
    readdirSync(d).flatMap((f) => {
      const p = join(d, f);
      return statSync(p).isDirectory() ? walk(p) : /\.(ts|tsx)$/.test(f) && !/\.test\.tsx?$/.test(f) ? [p] : [];
    });
  it("has no toFixed( next to a dollar sign outside lib/money.ts", () => {
    const bad: string[] = [];
    for (const f of walk(root)) {
      const rel = relative(root, f);
      if (rel === join("lib", "money.ts")) continue;
      readFileSync(f, "utf8")
        .split("\n")
        .forEach((line, i) => {
          if (/toFixed\(/.test(line) && /\$\$\{|"\$[0-9"]|\busd\b|\bcost/i.test(line)) bad.push(`${rel}:${i + 1}: ${line.trim()}`);
        });
    }
    expect(bad).toEqual([]);
  });
});
