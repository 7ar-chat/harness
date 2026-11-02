// Package triggers creates factory tasks from the outside: a cron scheduler
// that fires stored cron triggers, and an HTTP handler for GitHub webhooks
// that normalizes each delivery into a factory.Event and creates the tasks of
// the webhook triggers it matches. Both are idempotent per source key, so
// several processes (or a redelivery) never create a task twice.
package triggers

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed cron expression or "@every" interval.
type Schedule struct {
	expr  string
	every time.Duration // > 0: fixed interval

	minute, hour, dom, month, dow uint64 // bit sets
	// domAny and dowAny record a leading '*' in the day fields: when both day
	// fields are restricted, a day matches if either does (Vixie cron).
	domAny, dowAny bool
}

// String is the expression the schedule was parsed from.
func (s Schedule) String() string { return s.expr }

// farthest is how many years Next looks ahead: far enough for February 29th
// that must also be a Monday.
const farthest = 40

var macros = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

var (
	monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
	dowNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}
)

// ParseSchedule parses a five-field cron expression (minute hour day-of-month
// month day-of-week; lists, ranges, steps, month and weekday names, "*"),
// "@every <duration>", or one of @hourly, @daily, @midnight, @weekly,
// @monthly, @yearly.
func ParseSchedule(expr string) (Schedule, error) {
	e := strings.TrimSpace(expr)
	if e == "" {
		return Schedule{}, fmt.Errorf("schedule: empty expression")
	}
	if rest, ok := strings.CutPrefix(e, "@every"); ok {
		d, err := time.ParseDuration(strings.TrimSpace(rest))
		if err != nil || d <= 0 {
			return Schedule{}, fmt.Errorf("schedule %q: @every needs a positive duration like 90s or 2h", expr)
		}
		return Schedule{expr: e, every: d}, nil
	}
	if m, ok := macros[strings.ToLower(e)]; ok {
		s, err := parseCron(m)
		s.expr = e
		return s, err
	}
	s, err := parseCron(e)
	if err != nil {
		return Schedule{}, fmt.Errorf("schedule %q: %w", expr, err)
	}
	s.expr = e
	// A date that never occurs (31 February) would be stored and never fire.
	if s.Next(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)).IsZero() {
		return Schedule{}, fmt.Errorf("schedule %q: never fires", expr)
	}
	return s, nil
}

func parseCron(e string) (Schedule, error) {
	f := strings.Fields(e)
	if len(f) != 5 {
		return Schedule{}, fmt.Errorf("want 5 fields (minute hour day-of-month month day-of-week), got %d", len(f))
	}
	var s Schedule
	var err error
	if s.minute, err = parseField(f[0], 0, 59, nil); err != nil {
		return Schedule{}, fmt.Errorf("minute: %w", err)
	}
	if s.hour, err = parseField(f[1], 0, 23, nil); err != nil {
		return Schedule{}, fmt.Errorf("hour: %w", err)
	}
	if s.dom, err = parseField(f[2], 1, 31, nil); err != nil {
		return Schedule{}, fmt.Errorf("day of month: %w", err)
	}
	if s.month, err = parseField(f[3], 1, 12, monthNames); err != nil {
		return Schedule{}, fmt.Errorf("month: %w", err)
	}
	if s.dow, err = parseField(f[4], 0, 7, dowNames); err != nil {
		return Schedule{}, fmt.Errorf("day of week: %w", err)
	}
	if s.dow&(1<<7) != 0 { // 7 is Sunday too
		s.dow = s.dow&^(1<<7) | 1
	}
	s.domAny = strings.HasPrefix(f[2], "*")
	s.dowAny = strings.HasPrefix(f[4], "*")
	return s, nil
}

// parseField parses one comma list of "*", "n", "a-b", each with an optional "/step".
func parseField(field string, lo, hi int, names map[string]int) (uint64, error) {
	var set uint64
	for _, part := range strings.Split(field, ",") {
		if part == "" {
			return 0, fmt.Errorf("empty list item in %q", field)
		}
		rng, stepText, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepText)
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("bad step %q", stepText)
			}
			step = n
		}
		from, to := lo, hi
		switch {
		case rng == "*":
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var err error
			if from, err = parseValue(a, lo, hi, names); err != nil {
				return 0, err
			}
			if to, err = parseValue(b, lo, hi, names); err != nil {
				return 0, err
			}
			if from > to {
				return 0, fmt.Errorf("range %q runs backwards", rng)
			}
		default:
			v, err := parseValue(rng, lo, hi, names)
			if err != nil {
				return 0, err
			}
			from, to = v, v
			if hasStep { // "5/10" means 5 to the end in steps of 10
				to = hi
			}
		}
		for v := from; v <= to; v += step {
			set |= 1 << uint(v)
		}
	}
	return set, nil
}

func parseValue(s string, lo, hi int, names map[string]int) (int, error) {
	if v, ok := names[strings.ToLower(s)]; ok {
		return v, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("bad value %q", s)
	}
	if v < lo || v > hi {
		return 0, fmt.Errorf("%d is outside %d-%d", v, lo, hi)
	}
	return v, nil
}

func has(set uint64, v int) bool { return set&(1<<uint(v)) != 0 }

// Next is the first fire time strictly after after, in UTC; the zero Time when
// the schedule never fires again.
func (s Schedule) Next(after time.Time) time.Time {
	if s.every > 0 {
		return after.UTC().Add(s.every)
	}
	t := after.UTC().Truncate(time.Minute).Add(time.Minute)
	limit := t.Year() + farthest
	for t.Year() <= limit {
		switch {
		case !has(s.month, int(t.Month())):
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		case !s.dayMatches(t):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
		case !has(s.hour, t.Hour()):
			t = t.Truncate(time.Hour).Add(time.Hour)
		case !has(s.minute, t.Minute()):
			t = t.Add(time.Minute)
		default:
			return t
		}
	}
	return time.Time{}
}

func (s Schedule) dayMatches(t time.Time) bool {
	dom, dow := has(s.dom, t.Day()), has(s.dow, int(t.Weekday()))
	if !s.domAny && !s.dowAny {
		return dom || dow
	}
	return dom && dow
}
