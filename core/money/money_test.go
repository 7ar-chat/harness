package money

import (
	"math"
	"strings"
	"testing"
)

func TestFormatUSD(t *testing.T) {
	cases := []struct {
		in      float64
		want    string
		compact string
	}{
		{math.NaN(), "n/a", "n/a"},
		{math.Inf(1), "n/a", "n/a"},
		{0, "$0", "$0"},
		{0.00001, "<$0.0001", "<$0.0001"},
		{0.0001, "$0.0001", "$0.0001"},
		{0.0003, "$0.0003", "$0.0003"},
		{0.003, "$0.003", "$0.003"},
		{0.0099, "$0.0099", "$0.0099"},
		{0.01, "$0.01", "$0.01"},
		{0.03, "$0.03", "$0.03"},
		{0.0300001, "$0.03", "$0.03"},
		{0.5, "$0.50", "$0.50"},
		{1, "$1.00", "$1.00"},
		{12.5, "$12.50", "$12.50"},
		{999.99, "$999.99", "$999.99"},
		{999.999, "$1,000.00", "$1,000.00"},
		{1000, "$1,000.00", "$1k"},
		{1234.5, "$1,234.50", "$1.2k"},
		{1234567.891, "$1,234,567.89", "$1.2M"},
		{3400000, "$3,400,000.00", "$3.4M"},
		{-0.5, "-$0.50", "-$0.50"},
		{-0.0003, "-$0.0003", "-$0.0003"},
	}
	for _, c := range cases {
		if got := FormatUSD(c.in); got != c.want {
			t.Errorf("FormatUSD(%v) = %q, want %q", c.in, got, c.want)
		}
		if got := FormatUSDCompact(c.in); got != c.compact {
			t.Errorf("FormatUSDCompact(%v) = %q, want %q", c.in, got, c.compact)
		}
	}
	for _, n := range []float64{1e-7, 5e-5, 1e9, 1e21} {
		if s := FormatUSD(n); strings.ContainsAny(s, "eE") {
			t.Errorf("FormatUSD(%v) = %q looks scientific", n, s)
		}
	}
}
