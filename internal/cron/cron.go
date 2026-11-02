package cron

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxExpressionBytes = 256

const maxSearchDays = 4 * 366

type field struct {
	allowed    [60]bool
	restricted bool
}

type Schedule struct {
	minute  field
	hour    field
	day     field
	month   field
	weekday field
}

func Parse(expression string) (Schedule, error) {
	if len(expression) > maxExpressionBytes {
		return Schedule{}, fmt.Errorf("cron: expression exceeds %d bytes", maxExpressionBytes)
	}
	if strings.HasPrefix(strings.TrimSpace(expression), "@") {
		return Schedule{}, fmt.Errorf("cron: %q is a shorthand this parser does not accept; write five fields",
			strings.TrimSpace(expression))
	}
	parts := strings.Fields(expression)
	if len(parts) != 5 {
		return Schedule{}, fmt.Errorf("cron: %q has %d fields, want exactly 5 (minute hour day-of-month month day-of-week)",
			expression, len(parts))
	}
	var schedule Schedule
	specs := []struct {
		name      string
		raw       string
		low       int
		high      int
		target    *field
		wrapSeven bool
	}{
		{"minute", parts[0], 0, 59, &schedule.minute, false},
		{"hour", parts[1], 0, 23, &schedule.hour, false},
		{"day-of-month", parts[2], 1, 31, &schedule.day, false},
		{"month", parts[3], 1, 12, &schedule.month, false},
		{"day-of-week", parts[4], 0, 7, &schedule.weekday, true},
	}
	for _, spec := range specs {
		parsed, err := parseField(spec.name, spec.raw, spec.low, spec.high, spec.wrapSeven)
		if err != nil {
			return Schedule{}, err
		}
		*spec.target = parsed
	}
	return schedule, nil
}

func parseField(name, raw string, low, high int, wrapSeven bool) (field, error) {
	var result field
	if raw == "" {
		return field{}, fmt.Errorf("cron: %s field is empty", name)
	}
	result.restricted = !strings.HasPrefix(raw, "*")
	for _, item := range strings.Split(raw, ",") {
		if item == "" {
			return field{}, fmt.Errorf("cron: %s field %q has an empty entry", name, raw)
		}
		base, step := item, 1
		if slash := strings.Index(item, "/"); slash >= 0 {
			base = item[:slash]
			stepText := item[slash+1:]
			if strings.Contains(stepText, "/") {
				return field{}, fmt.Errorf("cron: %s entry %q has more than one step", name, item)
			}
			value, err := strconv.Atoi(stepText)
			if err != nil || value < 1 {
				return field{}, fmt.Errorf("cron: %s entry %q has an invalid step", name, item)
			}
			step = value
		}
		start, end := low, high
		switch {
		case base == "*":
		case base == "":
			return field{}, fmt.Errorf("cron: %s entry %q has no value before its step", name, item)
		case strings.Contains(base, "-"):
			bounds := strings.Split(base, "-")
			if len(bounds) != 2 || bounds[0] == "" || bounds[1] == "" {
				return field{}, fmt.Errorf("cron: %s entry %q is not a range", name, item)
			}
			first, err := strconv.Atoi(bounds[0])
			if err != nil {
				return field{}, fmt.Errorf("cron: %s entry %q is not numeric", name, item)
			}
			last, err := strconv.Atoi(bounds[1])
			if err != nil {
				return field{}, fmt.Errorf("cron: %s entry %q is not numeric", name, item)
			}
			if first > last {
				return field{}, fmt.Errorf("cron: %s entry %q counts backwards", name, item)
			}
			start, end = first, last
		default:
			value, err := strconv.Atoi(base)
			if err != nil {
				return field{}, fmt.Errorf("cron: %s entry %q is not numeric", name, item)
			}
			start, end = value, value
			if strings.Contains(item, "/") {
				end = high
			}
		}
		if start < low || end > high {
			return field{}, fmt.Errorf("cron: %s entry %q is outside %d-%d", name, item, low, high)
		}
		for value := start; value <= end; value += step {
			slot := value
			if wrapSeven && slot == 7 {
				slot = 0
			}
			result.allowed[slot] = true
			if step > end-value {
				break
			}
		}
	}
	return result, nil
}

func (f field) matches(value int) bool { return f.allowed[value] }

func (schedule Schedule) Next(after time.Time) (time.Time, error) {
	candidate := after.UTC().Truncate(time.Minute).Add(time.Minute)
	limit := candidate.AddDate(0, 0, maxSearchDays)
	for candidate.Before(limit) {
		if !schedule.month.matches(int(candidate.Month())) {
			candidate = time.Date(candidate.Year(), candidate.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
			continue
		}
		if !schedule.dayMatches(candidate) {
			candidate = time.Date(candidate.Year(), candidate.Month(), candidate.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
			continue
		}
		if !schedule.hour.matches(candidate.Hour()) {
			candidate = candidate.Truncate(time.Hour).Add(time.Hour)
			continue
		}
		if !schedule.minute.matches(candidate.Minute()) {
			candidate = candidate.Add(time.Minute)
			continue
		}
		return candidate, nil
	}
	return time.Time{}, fmt.Errorf("cron: no occurrence within %d days of %s",
		maxSearchDays, after.UTC().Format(time.RFC3339))
}

func (schedule Schedule) dayMatches(candidate time.Time) bool {
	dayOK := schedule.day.matches(candidate.Day())
	weekdayOK := schedule.weekday.matches(int(candidate.Weekday()))
	if schedule.day.restricted && schedule.weekday.restricted {
		return dayOK || weekdayOK
	}
	return dayOK && weekdayOK
}

// Interval checks time-of-day gaps and the full Gregorian calendar cycle.
// UTC dates and weekdays repeat after 400 years (146097 days). Checking one
// cycle plus its wraparound covers leap years and month/year boundaries.
type intervalResult struct {
	duration time.Duration
	err      error
}

var intervalCache = struct {
	sync.Mutex
	values map[Schedule]intervalResult
}{values: make(map[Schedule]intervalResult)}

const maximumIntervalCacheEntries = 1024

// The minimum gap is independent of the caller's starting time. Cache the
// normalized masks and bound memory even when many different expressions arrive.
func (schedule Schedule) Interval(_ time.Time) (time.Duration, error) {
	intervalCache.Lock()
	defer intervalCache.Unlock()
	if value, ok := intervalCache.values[schedule]; ok {
		return value.duration, value.err
	}
	duration, err := schedule.calendarInterval()
	if len(intervalCache.values) >= maximumIntervalCacheEntries {
		clear(intervalCache.values)
	}
	intervalCache.values[schedule] = intervalResult{duration, err}
	return duration, err
}
func (schedule Schedule) calendarInterval() (time.Duration, error) {
	var times []time.Duration
	for hour := 0; hour < 24; hour++ {
		for minute := 0; minute < 60; minute++ {
			if schedule.hour.matches(hour) && schedule.minute.matches(minute) {
				times = append(times, time.Duration(hour*60+minute)*time.Minute)
			}
		}
	}
	if len(times) == 0 {
		return 0, fmt.Errorf("cron: no allowed time of day")
	}
	smallest := time.Duration(0)
	consider := func(gap time.Duration) {
		if smallest == 0 || gap < smallest {
			smallest = gap
		}
	}
	for i := 1; i < len(times); i++ {
		consider(times[i] - times[i-1])
	}
	day := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	var first, previous time.Time
	for i := 0; i < 146097; i++ {
		if schedule.month.matches(int(day.Month())) && schedule.dayMatches(day) {
			if first.IsZero() {
				first = day
			}
			if !previous.IsZero() {
				consider(day.Add(times[0]).Sub(previous.Add(times[len(times)-1])))
			}
			previous = day
		}
		day = day.AddDate(0, 0, 1)
	}
	if first.IsZero() {
		return 0, fmt.Errorf("cron: no occurrence in the Gregorian calendar cycle")
	}
	consider(first.AddDate(400, 0, 0).Add(times[0]).Sub(previous.Add(times[len(times)-1])))
	return smallest, nil
}
