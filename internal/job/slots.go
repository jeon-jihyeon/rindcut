package job

import (
	"fmt"
	"time"
)

// Slot is one scheduled run of a job
type Slot struct {
	JobID string
	At    time.Time
	// End closes the window the run may take, At plus the job's grace
	End time.Time
}

// Key names the slot in verdicts, stable across revisions and machines
func (s Slot) Key() string {
	return fmt.Sprintf("%s@%s", s.JobID, s.At.Format(time.RFC3339))
}

// Slots lists the job's scheduled runs in [from, to), oldest first
// 1. A time a DST gap skips has no slot
// 2. A time a DST overlap repeats has one slot, at its first occurrence
func (j Job) Slots(from, to time.Time) ([]Slot, error) {
	sched, err := parseSchedule(j.Schedule)
	if err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(j.Timezone)
	if err != nil {
		return nil, fmt.Errorf("timezone %q: %w", j.Timezone, err)
	}
	var out []Slot
	first := from.In(loc)
	day := time.Date(first.Year(), first.Month(), first.Day(), 0, 0, 0, 0, loc)
	for ; day.Before(to); day = day.AddDate(0, 0, 1) {
		if !sched.dayMatches(day.Day(), int(day.Month()), int(day.Weekday())) {
			continue
		}
		for h := range 24 {
			if sched.hours&(1<<h) == 0 {
				continue
			}
			for m := range 60 {
				if sched.minutes&(1<<m) == 0 {
					continue
				}
				at, shown := wallTime(day, h, m, loc)
				if !shown || at.Before(from) || !at.Before(to) {
					continue
				}
				out = append(out, Slot{JobID: j.ID, At: at, End: at.Add(time.Duration(j.Grace))})
			}
		}
	}
	return out, nil
}

// Next is the first slot after t
// A month ahead covers most schedules and a year the rest, so false means none fires within a year
func (j Job) Next(t time.Time) (Slot, bool) {
	from := t.Add(time.Nanosecond)
	for _, to := range []time.Time{from.AddDate(0, 1, 1), from.AddDate(1, 0, 1)} {
		slots, err := j.Slots(from, to)
		if err != nil {
			return Slot{}, false
		}
		if len(slots) > 0 {
			return slots[0], true
		}
	}
	return Slot{}, false
}

// wallTime is the instant a local clock shows h:m on day
// False when the clock never shows it because DST skipped it
func wallTime(day time.Time, h, m int, loc *time.Location) (time.Time, bool) {
	t := time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, loc)
	if t.Hour() != h || t.Minute() != m {
		return t, false
	}
	// In a DST overlap the same clock time happens twice
	// Go doesn't promise which one it picks, so step back to the first if an hour earlier reads the same
	if earlier := t.Add(-time.Hour); earlier.Hour() == h && earlier.Minute() == m && earlier.Day() == t.Day() {
		return earlier, true
	}
	return t, true
}
