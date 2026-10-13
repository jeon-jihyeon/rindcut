package job

import (
	"fmt"
	"strconv"
	"strings"
)

// schedule is a parsed 5-field cron line: minute, hour, day of month, month, day of week
type schedule struct {
	minutes, hours, days, months, weekdays uint64
	// Cron's own rule: when both day fields are restricted, a day matches either one
	daysRestricted, weekdaysRestricted bool
}

type field struct {
	name     string
	min, max int
	names    map[string]int
}

var (
	monthNames   = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
	weekdayNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}
	fields       = [5]field{
		{"minute", 0, 59, nil},
		{"hour", 0, 23, nil},
		{"day of month", 1, 31, nil},
		{"month", 1, 12, monthNames},
		{"day of week", 0, 7, weekdayNames},
	}
)

func parseSchedule(line string) (schedule, error) {
	parts := strings.Fields(line)
	if len(parts) != len(fields) {
		return schedule{}, fmt.Errorf("schedule %q needs 5 fields: minute hour day-of-month month day-of-week", line)
	}
	var sets [5]uint64
	for i, p := range parts {
		set, err := fields[i].parse(strings.ToLower(p))
		if err != nil {
			return schedule{}, fmt.Errorf("schedule %q: %w", line, err)
		}
		sets[i] = set
	}
	// 7 is another name for Sunday
	if sets[4]&(1<<7) != 0 {
		sets[4] = sets[4]&^(1<<7) | 1
	}
	return schedule{
		minutes: sets[0], hours: sets[1], days: sets[2], months: sets[3], weekdays: sets[4],
		daysRestricted: !strings.HasPrefix(parts[2], "*"), weekdaysRestricted: !strings.HasPrefix(parts[4], "*"),
	}, nil
}

// Parses one field: lists of `*`, `n`, `a-b`, each with an optional `/step`
func (f field) parse(text string) (uint64, error) {
	var set uint64
	for item := range strings.SplitSeq(text, ",") {
		rng, stepText, stepped := strings.Cut(item, "/")
		step := 1
		if stepped {
			n, err := strconv.Atoi(stepText)
			if err != nil || n < 1 {
				return 0, fmt.Errorf("%s step %q must be a positive number", f.name, stepText)
			}
			step = n
		}
		lo, hi, err := f.bounds(rng, stepped)
		if err != nil {
			return 0, err
		}
		for v := lo; v <= hi; v += step {
			set |= 1 << v
		}
	}
	return set, nil
}

func (f field) bounds(rng string, stepped bool) (int, int, error) {
	if rng == "*" {
		return f.min, f.max, nil
	}
	loText, hiText, isRange := strings.Cut(rng, "-")
	lo, err := f.value(loText)
	if err != nil {
		return 0, 0, err
	}
	hi := lo
	switch {
	case isRange:
		if hi, err = f.value(hiText); err != nil {
			return 0, 0, err
		}
	case stepped:
		// `5/15` runs from 5 to the end, as in Vixie cron
		hi = f.max
	}
	if lo > hi {
		return 0, 0, fmt.Errorf("%s range %q runs backwards", f.name, rng)
	}
	return lo, hi, nil
}

func (f field) value(text string) (int, error) {
	if v, ok := f.names[text]; ok {
		return v, nil
	}
	v, err := strconv.Atoi(text)
	if err != nil || v < f.min || v > f.max {
		return 0, fmt.Errorf("%s %q must be %d to %d", f.name, text, f.min, f.max)
	}
	return v, nil
}

func (s schedule) dayMatches(day, month, weekday int) bool {
	if s.months&(1<<month) == 0 {
		return false
	}
	inDays, inWeekdays := s.days&(1<<day) != 0, s.weekdays&(1<<weekday) != 0
	if s.daysRestricted && s.weekdaysRestricted {
		return inDays || inWeekdays
	}
	return inDays && inWeekdays
}
