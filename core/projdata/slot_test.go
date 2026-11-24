package projdata_test

import (
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/projdata"
)

func at(t *testing.T, zone, layout, v string) time.Time {
	t.Helper()
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	tm, err := time.ParseInLocation(layout, v, loc)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

const lay = "2006-01-02 15:04"

func TestDueInTheProjectsZone(t *testing.T) {
	// Monday 09:00 in Tokyo is Monday 00:00 UTC and Sunday 19:00 in New York.
	for _, c := range []struct {
		zone    string
		now     string
		nowZone string
		wantKey string
		due     bool
	}{
		{"Asia/Tokyo", "2026-11-23 09:30", "Asia/Tokyo", "2026-11-23", true},
		{"Asia/Tokyo", "2026-11-23 08:59", "Asia/Tokyo", "", false},   // before the hour: last week's slot is stale
		{"Asia/Tokyo", "2026-11-23 21:00", "Asia/Tokyo", "", false},   // 12h grace is over
		{"Asia/Tokyo", "2026-11-22 23:30", "UTC", "2026-11-23", true}, // the same instant as 08:30 Tokyo? no: 23:30 UTC = Monday 08:30 JST
		{"America/New_York", "2026-11-23 09:00", "America/New_York", "2026-11-23", true},
		{"", "2026-11-23 09:00", "UTC", "2026-11-23", true},
	} {
		s := projdata.DigestSettings{ProjectID: "p", Enabled: true, Weekday: 1, Hour: 9, Timezone: c.zone, EnabledAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
		now := at(t, c.nowZone, lay, c.now)
		sl, ok := s.Due(now)
		if c.zone == "Asia/Tokyo" && c.now == "2026-11-22 23:30" {
			// 23:30 UTC on Sunday is 08:30 Monday in Tokyo, before 09:00: nothing due.
			if ok {
				t.Fatalf("due before the hour in Tokyo: %+v", sl)
			}
			continue
		}
		if ok != c.due || (ok && sl.Key != c.wantKey) {
			t.Errorf("%s now %s (%s): due=%v key=%q, want due=%v key=%q", c.zone, c.now, c.nowZone, ok, sl.Key, c.due, c.wantKey)
		}
	}
}

func TestDueRespectsLastSlotAndEnabledAt(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Berlin")
	now := time.Date(2026, 11, 23, 9, 30, 0, 0, loc)
	s := projdata.DigestSettings{ProjectID: "p", Enabled: true, Weekday: 1, Hour: 9, Timezone: "Europe/Berlin", EnabledAt: now.Add(-48 * time.Hour)}
	if _, ok := s.Due(now); !ok {
		t.Fatal("not due")
	}
	s.LastSlot = "2026-11-23"
	if _, ok := s.Due(now); ok {
		t.Fatal("due again after the slot was handled")
	}
	s.LastSlot = ""
	s.EnabledAt = now // switched on after the hour: this week's slot is in the past
	if _, ok := s.Due(now); ok {
		t.Fatal("a slot before the digest was switched on")
	}
	s.EnabledAt = now.Add(-time.Hour)
	if _, ok := s.Due(now); !ok {
		t.Fatal("switched on before the hour, should be due")
	}
	s.Enabled = false
	if _, ok := s.Due(now); ok {
		t.Fatal("due while off")
	}
}

func TestSlotRangeIsSevenLocalDaysAcrossDST(t *testing.T) {
	// Europe/Berlin leaves summer time on Sunday 2026-10-25; the slot Monday 2026-10-26 09:00 covers a week of
	// 7*24+1 real hours, still seven calendar days.
	loc, _ := time.LoadLocation("Europe/Berlin")
	s := projdata.DigestSettings{ProjectID: "p", Enabled: true, Weekday: 1, Hour: 9, Timezone: "Europe/Berlin", EnabledAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	sl, ok := s.Due(time.Date(2026, 10, 26, 9, 5, 0, 0, loc))
	if !ok || sl.Key != "2026-10-26" {
		t.Fatalf("slot = %+v ok=%v", sl, ok)
	}
	if got := sl.At.In(loc).Format(lay); got != "2026-10-26 09:00" {
		t.Fatalf("at = %s", got)
	}
	if got := sl.From.In(loc).Format(lay); got != "2026-10-19 09:00" {
		t.Fatalf("from = %s", got)
	}
	if d := sl.At.Sub(sl.From); d != 169*time.Hour {
		t.Fatalf("the week lasted %v", d)
	}
	// The spring change (2026-03-29) makes the week 167 hours.
	sl, ok = s.Due(time.Date(2026, 3, 30, 9, 1, 0, 0, loc))
	if !ok || sl.At.Sub(sl.From) != 167*time.Hour {
		t.Fatalf("spring week = %+v", sl)
	}
}

func TestNonexistentAndRepeatedHoursStillGiveOneSlotPerWeek(t *testing.T) {
	// Sunday 2026-03-29 02:00 does not exist in Berlin; Sunday 2026-10-25 02:00 happens twice.
	loc, _ := time.LoadLocation("Europe/Berlin")
	s := projdata.DigestSettings{ProjectID: "p", Enabled: true, Weekday: 0, Hour: 2, Timezone: "Europe/Berlin", EnabledAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	for _, day := range []string{"2026-03-29", "2026-10-25"} {
		keys := map[string]bool{}
		start := at(t, "Europe/Berlin", lay, day+" 00:30")
		for m := 0; m < 6*60; m += 10 {
			now := start.Add(time.Duration(m) * time.Minute)
			if sl, ok := s.Due(now); ok {
				keys[sl.Key] = true
				if sl.Key != day {
					t.Fatalf("%s: key %s", day, sl.Key)
				}
			}
		}
		if len(keys) != 1 {
			t.Fatalf("%s: slots %v (location %v)", day, keys, loc)
		}
	}
}

func TestNextIsOneWeekAfterTheLatestSlot(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Tokyo")
	s := projdata.DigestSettings{ProjectID: "p", Enabled: true, Weekday: 5, Hour: 18, Timezone: "Asia/Tokyo"}
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, loc) // Tuesday
	next, ok := s.Next(now)
	if !ok || next.In(loc).Format(lay) != "2026-11-27 18:00" {
		t.Fatalf("next = %v ok=%v", next, ok)
	}
	next, _ = s.Next(time.Date(2026, 11, 27, 19, 0, 0, 0, loc))
	if next.In(loc).Format(lay) != "2026-12-04 18:00" {
		t.Fatalf("next = %v", next)
	}
	s.Enabled = false
	if _, ok := s.Next(now); ok {
		t.Fatal("next while off")
	}
}

func TestValidate(t *testing.T) {
	for _, s := range []projdata.DigestSettings{{Weekday: 7}, {Weekday: -1}, {Hour: 24}, {Hour: -1}, {Timezone: "Mars/Base"}} {
		if s.Validate() == nil {
			t.Errorf("%+v accepted", s)
		}
	}
	if err := (projdata.DigestSettings{Weekday: 6, Hour: 23, Timezone: "Europe/Paris"}).Validate(); err != nil {
		t.Fatal(err)
	}
}
