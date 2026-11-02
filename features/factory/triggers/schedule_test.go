package triggers_test

import (
	"testing"
	"time"

	"github.com/7ar-chat/harness/features/factory/triggers"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestScheduleNext(t *testing.T) {
	tests := []struct {
		name, expr, after, want string
	}{
		{"every minute", "* * * * *", "2026-10-31T12:00:00Z", "2026-10-31T12:01:00Z"},
		{"strictly after an exact fire time", "30 2 * * *", "2026-10-31T02:30:00Z", "2026-11-01T02:30:00Z"},
		{"before the fire time today", "30 2 * * *", "2026-10-31T01:00:00Z", "2026-10-31T02:30:00Z"},
		{"seconds are dropped", "0 * * * *", "2026-10-31T12:00:59Z", "2026-10-31T13:00:00Z"},
		{"month end skips short months", "0 0 31 * *", "2026-04-15T00:00:00Z", "2026-05-31T00:00:00Z"},
		{"month end after 30th", "0 0 31 * *", "2026-01-31T00:00:00Z", "2026-03-31T00:00:00Z"},
		{"feb 29 waits for a leap year", "0 0 29 2 *", "2026-03-01T00:00:00Z", "2028-02-29T00:00:00Z"},
		{"dow only", "0 9 * * mon", "2026-10-31T12:00:00Z", "2026-11-02T09:00:00Z"},
		{"sunday as 0 and 7", "0 9 * * 7", "2026-10-31T12:00:00Z", "2026-11-01T09:00:00Z"},
		{"dom and dow both restricted: either matches (dow first)", "0 0 15 * fri", "2026-10-31T12:00:00Z", "2026-11-06T00:00:00Z"},
		{"dom and dow both restricted: either matches (dom first)", "0 0 1 * fri", "2026-10-28T12:00:00Z", "2026-10-30T00:00:00Z"},
		{"dom restricted with star dow", "0 0 15 * *", "2026-10-31T12:00:00Z", "2026-11-15T00:00:00Z"},
		{"list range and step", "5,35 8-10/2 * * *", "2026-10-31T08:35:00Z", "2026-10-31T10:05:00Z"},
		{"step from a start", "10/20 * * * *", "2026-10-31T12:30:00Z", "2026-10-31T12:50:00Z"},
		{"star step", "*/15 * * * *", "2026-10-31T12:46:00Z", "2026-10-31T13:00:00Z"},
		{"month names", "0 0 1 jan,jul *", "2026-02-01T00:00:00Z", "2026-07-01T00:00:00Z"},
		{"dow range names", "0 8 * * mon-fri", "2026-10-30T09:00:00Z", "2026-11-02T08:00:00Z"},
		{"year rollover", "0 0 1 1 *", "2026-12-31T23:59:00Z", "2027-01-01T00:00:00Z"},
		{"non UTC input is converted", "0 0 * * *", "2026-10-31T23:30:00-05:00", "2026-11-02T00:00:00Z"},
		{"@hourly", "@hourly", "2026-10-31T12:34:00Z", "2026-10-31T13:00:00Z"},
		{"@daily", "@daily", "2026-10-31T12:34:00Z", "2026-11-01T00:00:00Z"},
		{"@weekly", "@weekly", "2026-10-31T12:34:00Z", "2026-11-01T00:00:00Z"},
		{"@monthly", "@monthly", "2026-10-31T12:34:00Z", "2026-11-01T00:00:00Z"},
		{"@every keeps the offset from after", "@every 90s", "2026-10-31T12:00:10Z", "2026-10-31T12:01:40Z"},
		{"@every hours", "@every 2h", "2026-10-31T12:00:00Z", "2026-10-31T14:00:00Z"},
		{"surrounding spaces", "  0   0 * * *  ", "2026-10-31T12:00:00Z", "2026-11-01T00:00:00Z"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			s, err := triggers.ParseSchedule(tc.expr)
			if err != nil {
				t.Fatalf("ParseSchedule(%q): %v", tc.expr, err)
			}

			// Act
			got := s.Next(at(tc.after))

			// Assert
			if !got.Equal(at(tc.want)) || got.Location() != time.UTC {
				t.Errorf("Next(%s) = %v, want %s UTC", tc.after, got, tc.want)
			}
		})
	}
}

func TestParseScheduleRejectsInvalidExpressions(t *testing.T) {
	for _, expr := range []string{
		"", "   ", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * 32 * *", "* * * 13 *",
		"* * * * 8", "a * * * *", "5-1 * * * *", "*/0 * * * *", "*/x * * * *", "1,,2 * * * *", "* * * foo *",
		"@every", "@every 0s", "@every -1h", "@every soon", "@sometimes",
		"0 0 31 2 *", // 31 February never occurs
	} {
		t.Run(expr, func(t *testing.T) {
			// Act
			_, err := triggers.ParseSchedule(expr)

			// Assert
			if err == nil {
				t.Errorf("ParseSchedule(%q) accepted an invalid expression", expr)
			}
		})
	}
}

func TestScheduleStringIsTheExpression(t *testing.T) {
	// Arrange
	s, err := triggers.ParseSchedule(" @daily ")
	if err != nil {
		t.Fatal(err)
	}

	// Act + Assert
	if s.String() != "@daily" {
		t.Errorf("String = %q", s.String())
	}
}
