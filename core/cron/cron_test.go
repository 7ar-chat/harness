package cron

import (
	"testing"
	"time"
)

func TestParseAndNextInUTC(t *testing.T) {
	from := time.Date(2026, 11, 3, 12, 7, 30, 0, time.UTC)
	for expr, want := range map[string]time.Time{
		"*/15 * * * *":  time.Date(2026, 11, 3, 12, 15, 0, 0, time.UTC),
		"0 0 * * *":     time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC),
		"@hourly":       time.Date(2026, 11, 3, 13, 0, 0, 0, time.UTC),
		"30 9 * * mon":  time.Date(2026, 11, 9, 9, 30, 0, 0, time.UTC),
		"0 0 1 jan *":   time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
		"@every 90m":    from.Add(90 * time.Minute),
		"0 12 29 2 *":   time.Date(2028, 2, 29, 12, 0, 0, 0, time.UTC),
		"5-10/5 12 * *": time.Time{},
	} {
		s, err := ParseSchedule(expr)
		if want.IsZero() {
			if err == nil {
				t.Errorf("%q parsed", expr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", expr, err)
			continue
		}
		if got := s.Next(from); !got.Equal(want) {
			t.Errorf("%q: next = %v, want %v", expr, got, want)
		}
	}
	for _, bad := range []string{"", "@every", "@every -1h", "60 * * * *", "* * * *", "0 0 31 2 *", "1-0 * * * *"} {
		if _, err := ParseSchedule(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestNextInReadsTheFieldsInTheZone(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Skip("no tzdata")
	}
	s, _ := ParseSchedule("0 9 * * *")
	// The day clocks go back (25 Oct 2026): 09:00 Paris is 07:00 UTC the day before, 08:00 UTC after.
	got := s.NextIn(time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC), paris)
	if want := time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("next = %v, want %v", got, want)
	}
	if got := s.NextIn(time.Date(2026, 10, 24, 5, 0, 0, 0, time.UTC), paris); !got.Equal(time.Date(2026, 10, 24, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("before the change: %v", got)
	}
	if got := s.NextIn(time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC), nil); !got.Equal(time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("nil zone is UTC: %v", got)
	}
	if s.String() != "0 9 * * *" {
		t.Fatal(s.String())
	}
}
