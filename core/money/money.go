// Package money formats dollar amounts the one way the product shows them. The web twin is web/src/lib/money.ts;
// both have a table test with the same cases.
package money

import (
	"math"
	"strconv"
	"strings"
)

// Unknown is what an unpriced or unknown amount reads as; it is never shown as a fake $0.
const Unknown = "n/a"

// FormatUSD formats dollars: "$0" for zero, up to four decimals (trailing zeros trimmed) under a cent,
// two decimals under $1000, thousands separators from there. NaN and infinities read "n/a".
func FormatUSD(usd float64) string { return format(usd, false) }

// FormatUSDCompact is FormatUSD with "$1.2k" and "$3.4M" for large amounts.
func FormatUSDCompact(usd float64) string { return format(usd, true) }

func format(n float64, compact bool) string {
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return Unknown
	}
	if n == 0 {
		return "$0"
	}
	sign := ""
	if n < 0 {
		sign = "-"
	}
	a := math.Abs(n)
	switch {
	case a < 0.00005:
		return sign + "<$0.0001"
	case a < 0.01:
		return sign + "$" + trim(strconv.FormatFloat(a, 'f', 4, 64))
	case compact && a >= 1e6:
		return sign + "$" + trim(strconv.FormatFloat(a/1e6, 'f', 1, 64)) + "M"
	case compact && a >= 1000:
		return sign + "$" + trim(strconv.FormatFloat(a/1000, 'f', 1, 64)) + "k"
	}
	two := strconv.FormatFloat(a, 'f', 2, 64)
	if v, _ := strconv.ParseFloat(two, 64); v < 1000 {
		return sign + "$" + two
	}
	whole, frac, _ := strings.Cut(two, ".")
	var b strings.Builder
	for i, r := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return sign + "$" + b.String() + "." + frac
}

func trim(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	return strings.TrimSuffix(strings.TrimRight(s, "0"), ".")
}
