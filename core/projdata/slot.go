package projdata

import (
	"errors"
	"time"
)

// DueGrace is how long after its time a digest slot may still be sent (a server that was down at the hour catches up
// within it; a later start does not send a stale digest).
const DueGrace = 12 * time.Hour

// Slot is one weekly occurrence of a digest: Key (the local date, "2026-11-23") identifies it, At is the instant the
// digest is due, and the digest covers From to At (the seven days before, in the project's zone).
type Slot struct {
	Key  string
	At   time.Time
	From time.Time
	Loc  *time.Location
}

// Location returns the zone of the setting ("" is UTC).
func (s DigestSettings) Location() (*time.Location, error) {
	if s.Timezone == "" {
		return time.UTC, nil
	}
	return time.LoadLocation(s.Timezone)
}

// Validate checks a setting a person submitted.
func (s DigestSettings) Validate() error {
	if s.Weekday < 0 || s.Weekday > 6 {
		return errors.New("weekday must be 0 (Sunday) to 6 (Saturday)")
	}
	if s.Hour < 0 || s.Hour > 23 {
		return errors.New("hour must be between 0 and 23")
	}
	if _, err := s.Location(); err != nil {
		return errors.New("timezone must be an IANA time zone such as Europe/Berlin")
	}
	return nil
}

// slotAt builds the slot whose local date is the given day of loc.
func (s DigestSettings) slotAt(loc *time.Location, y int, m time.Month, d int) Slot {
	at := time.Date(y, m, d, s.Hour, 0, 0, 0, loc)
	local := at.In(loc)
	return Slot{Key: time.Date(y, m, d, 0, 0, 0, 0, loc).Format("2006-01-02"), At: at, From: local.AddDate(0, 0, -7), Loc: loc}
}

// latest returns the most recent slot at or before now. The local calendar decides the day, so a daylight saving
// change moves the instant, never the date; an hour that does not exist that day (the spring gap) is the instant Go
// resolves it to, one that exists twice (the autumn repeat) is one instant, so a week has one slot either way.
func (s DigestSettings) latest(now time.Time, loc *time.Location) Slot {
	local := now.In(loc)
	days := (int(local.Weekday()) - s.Weekday + 7) % 7
	sl := s.slotAt(loc, local.Year(), local.Month(), local.Day()-days)
	if sl.At.After(now) {
		sl = s.slotAt(loc, local.Year(), local.Month(), local.Day()-days-7)
	}
	return sl
}

// Due returns the slot to send at now: the latest one, if it is within DueGrace, was not sent yet (LastSlot), and is
// after the digest was switched on. ok is false when nothing is due (also for a setting that is off or invalid).
func (s DigestSettings) Due(now time.Time) (Slot, bool) {
	if !s.Enabled || s.Validate() != nil {
		return Slot{}, false
	}
	loc, _ := s.Location()
	sl := s.latest(now, loc)
	if now.Sub(sl.At) >= DueGrace || sl.Key == s.LastSlot || !sl.At.After(s.EnabledAt) {
		return Slot{}, false
	}
	return sl, true
}

// Next returns the first slot after now, the time the next scheduled digest goes out.
func (s DigestSettings) Next(now time.Time) (time.Time, bool) {
	if !s.Enabled || s.Validate() != nil {
		return time.Time{}, false
	}
	loc, _ := s.Location()
	local := s.latest(now, loc).At.In(loc)
	return s.slotAt(loc, local.Year(), local.Month(), local.Day()+7).At, true
}

// ManualRange is the range of a digest sent on request: the seven days up to now in the zone of the setting.
func (s DigestSettings) ManualRange(now time.Time) (from, to time.Time, loc *time.Location) {
	loc, err := s.Location()
	if err != nil {
		loc = time.UTC
	}
	return now.In(loc).AddDate(0, 0, -7), now, loc
}
