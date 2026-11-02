package factory_test

import (
	"testing"

	"github.com/7ar-chat/harness/core/factory"
)

func TestFingerprintIgnoresWhatChangesBetweenRunsOfTheSameFailure(t *testing.T) {
	// Arrange: the same compiler error with a different line, commit, timestamp and spacing.
	a := "app.go:12: undefined: foo\n  at 3f9a1c2d4e (2026-10-31T10:00:01Z)"
	b := "APP.GO:97:   undefined: foo\n  at 0a0b0c0d0e1f (2026-11-02T23:59:59.123Z)"

	// Act / Assert
	if fa, fb := factory.Fingerprint(a), factory.Fingerprint(b); fa == "" || fa != fb {
		t.Errorf("fingerprints differ for the same failure: %q vs %q", fa, fb)
	}
}

func TestFingerprintSeparatesDifferentFailures(t *testing.T) {
	if factory.Fingerprint("undefined: foo") == factory.Fingerprint("cannot use x (variable of type int) as string") {
		t.Error("two different errors share a fingerprint")
	}
}

func TestFingerprintOfBlankTextIsEmptySoItNeverCountsAsRepeating(t *testing.T) {
	for _, s := range []string{"", "   \n\t "} {
		if got := factory.Fingerprint(s); got != "" {
			t.Errorf("Fingerprint(%q) = %q, want empty", s, got)
		}
	}
}

func TestFingerprintOnlyLooksAtTheHeadOfALongText(t *testing.T) {
	// Arrange: identical first 600 characters, different tails (a changing stack or log).
	head := ""
	for len(head) < 700 {
		head += "error: the build failed because of a type mismatch. "
	}

	// Act / Assert
	if factory.Fingerprint(head+"tail one") != factory.Fingerprint(head+"a different tail") {
		t.Error("a changing tail changed the fingerprint")
	}
}

func TestRepeating(t *testing.T) {
	cases := []struct {
		name string
		fps  []string
		n    int
		want bool
	}{
		{"three identical", []string{"a", "a", "a"}, 3, true},
		{"only the last n count", []string{"x", "b", "a", "a", "a"}, 3, true},
		{"alternating is not repeating", []string{"a", "b", "a", "b"}, 2, false},
		{"fewer than n", []string{"a", "a"}, 3, false},
		{"empty signatures never repeat", []string{"", "", ""}, 3, false},
		{"n below two is meaningless", []string{"a", "a", "a"}, 1, false},
		{"broken streak", []string{"a", "a", "b", "a"}, 3, false},
		{"none", nil, 3, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := factory.Repeating(c.fps, c.n); got != c.want {
				t.Errorf("Repeating(%v, %d) = %v, want %v", c.fps, c.n, got, c.want)
			}
		})
	}
}

func TestBudgetOverIsInclusiveAndPerAxis(t *testing.T) {
	cases := []struct {
		name string
		b    factory.Budget
		s    factory.Spend
		want bool
	}{
		{"no cap is never over", factory.Budget{}, factory.Spend{USD: 1e6, Tokens: 1 << 40}, false},
		{"exactly the dollar cap", factory.Budget{MaxUSD: 5}, factory.Spend{USD: 5}, true},
		{"just under", factory.Budget{MaxUSD: 5}, factory.Spend{USD: 4.99}, false},
		{"token cap only", factory.Budget{MaxTokens: 1000}, factory.Spend{Tokens: 1000, USD: 0}, true},
		{"either axis is enough", factory.Budget{MaxUSD: 5, MaxTokens: 1000}, factory.Spend{USD: 1, Tokens: 2000}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.b.Over(c.s); got != c.want {
				t.Errorf("Over = %v, want %v", got, c.want)
			}
		})
	}
}

func TestStateClassification(t *testing.T) {
	terminal := map[factory.State]bool{factory.StateDone: true, factory.StateFailed: true, factory.StateCancelled: true}
	for _, s := range []factory.State{factory.StatePending, factory.StateReady, factory.StateRunning, factory.StateReview,
		factory.StateBlocked, factory.StateDone, factory.StateFailed, factory.StateCancelled} {
		if !s.Valid() {
			t.Errorf("%q should be valid", s)
		}
		if s.Terminal() != terminal[s] {
			t.Errorf("%q Terminal = %v, want %v", s, s.Terminal(), terminal[s])
		}
	}
	if factory.State("bogus").Valid() || factory.State("").Valid() {
		t.Error("an unknown state is valid")
	}
}
