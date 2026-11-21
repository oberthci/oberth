package schedule

import (
	"errors"
	"strconv"
	"strings"
	"time"

	native "github.com/robfig/cron/v3"
)

// cronExpression holds native masks and syntax; it has no firing or retry loop.
type cronExpression struct {
	spec       native.Schedule
	expression string
}

// parseCron accepts five numeric fields, including Sunday 7, in the supplied timezone.
func parseCron(expression string, zone *time.Location) (cronExpression, error) {
	parts, err := fields(expression)
	if err != nil {
		return cronExpression{}, err
	}
	if zone == nil {
		zone = time.UTC
	}
	if strings.Contains(parts[4], "7") {
		normalized, err := sunday(parts[4])
		if err != nil {
			return cronExpression{}, err
		}
		parts[4] = normalized
	}
	expression = strings.Join(parts, " ")
	parsed, err := native.ParseStandard(expression)
	if err != nil {
		return cronExpression{}, err
	}
	// Numeric standard syntax always returns a native SpecSchedule.
	parsed.(*native.SpecSchedule).Location = zone
	return cronExpression{spec: parsed, expression: expression}, nil
}

// sunday uses the native minute mask to parse weekdays with the Sunday alias.
func sunday(field string) (string, error) {
	var mask uint64
	wildcard := false
	for _, item := range strings.Split(field, ",") {
		nativeItem, star := weekdayItem(item)
		wildcard = wildcard || star
		parsed, err := native.ParseStandard(nativeItem + " * * * *")
		if err != nil {
			return "", err
		}
		bits := parsed.(*native.SpecSchedule).Minute
		if bits > 255 {
			return "", errors.New("invalid cron weekday range")
		}
		mask |= bits&127 | bits>>7
	}
	if wildcard {
		return "*", nil
	}
	var days []string
	for day := 0; day < 7; day++ {
		if mask&(1<<uint(day)) != 0 {
			days = append(days, strconv.Itoa(day))
		}
	}
	return strings.Join(days, ","), nil
}

func fields(expression string) ([]string, error) {
	parts := strings.Fields(expression)
	if len(expression) > 256 || len(parts) != 5 || !numeric(expression) {
		return nil, errors.New("use five numeric cron fields within 256 bytes")
	}
	for _, part := range parts {
		if strings.HasPrefix(part, ",") || strings.HasSuffix(part, ",") || strings.Contains(part, ",,") {
			return nil, errors.New("cron lists cannot contain empty items")
		}
	}
	return parts, nil
}

func (s cronExpression) Expression() string { return s.expression }
func (s cronExpression) Next(after time.Time) time.Time {
	if s.spec == nil {
		return time.Time{}
	}
	return s.spec.Next(after)
}

func numeric(expression string) bool {
	for _, char := range expression {
		if (char < '0' || char > '9') && !strings.ContainsRune("*,-/ \t", char) {
			return false
		}
	}
	return true
}

func weekdayItem(item string) (string, bool) {
	bounds, step, stepped := strings.Cut(item, "/")
	stepValue, _ := strconv.Atoi(step)
	wildcard := bounds == "*" && (!stepped || stepValue == 1)
	if bounds == "*" {
		bounds = "0-7"
	} else if stepped && !strings.Contains(bounds, "-") {
		bounds += "-7"
	}
	if stepped {
		bounds += "/" + step
	}
	return bounds, wildcard
}
