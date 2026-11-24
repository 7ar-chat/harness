// The one place money is formatted in the web app. The Go twin is core/money.FormatUSD; keep the two in step
// (both have a table test with the same cases).
//
//   null / undefined / NaN  -> "n/a"        (unpriced or unknown, never a fake $0)
//   0                       -> "$0"
//   under $0.01             -> up to 4 decimals, trailing zeros trimmed ("$0.0003", "$0.003"); below $0.0001 "<$0.0001"
//   under $1000             -> two decimals ("$0.03", "$12.50")
//   $1000 and up            -> thousands separators ("$1,234.50"); compact: "$1.2k", "$3.4M"
// Never scientific notation.
export function formatUsd(n: number | null | undefined, opts: { compact?: boolean } = {}): string {
  if (n === null || n === undefined || !Number.isFinite(n)) return "n/a";
  if (n === 0) return "$0";
  const sign = n < 0 ? "-" : "";
  const a = Math.abs(n);
  if (a < 0.00005) return `${sign}<$0.0001`;
  if (a < 0.01) return `${sign}$${trim(a.toFixed(4))}`;
  if (opts.compact && a >= 1e6) return `${sign}$${trim((a / 1e6).toFixed(1))}M`;
  if (opts.compact && a >= 1000) return `${sign}$${trim((a / 1000).toFixed(1))}k`;
  const two = a.toFixed(2);
  if (Number(two) < 1000) return `${sign}$${two}`;
  return `${sign}$${a.toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`;
}

function trim(s: string): string {
  return s.includes(".") ? s.replace(/0+$/, "").replace(/\.$/, "") : s;
}
