package model

import (
	"math"
	"time"
)

// MetricCoverage counts distinct source timestamps for one metric, not polls
// that happened to refresh an unrelated metric. Zero means unknown coverage.
type MetricCoverage struct {
	Count   int64     `json:"count"`
	FirstAt time.Time `json:"first_at,omitzero"`
	LastAt  time.Time `json:"last_at,omitzero"`
}

func (c MetricCoverage) Valid(observed time.Time) bool {
	if c.Count == 0 {
		return c.FirstAt.IsZero() && c.LastAt.IsZero()
	}
	return c.Count > 0 && (c.Count == 1 || c.LastAt.After(c.FirstAt)) && !c.FirstAt.IsZero() && !c.LastAt.Before(c.FirstAt) && !c.LastAt.After(observed.Add(2*time.Second))
}

// Merge conservatively counts disjoint source intervals. Overlapping aggregate
// updates cannot prove how many timestamps are new; use their larger count.
func (c MetricCoverage) Merge(next MetricCoverage) MetricCoverage {
	if next.Count == 0 {
		return c
	}
	if c.Count == 0 {
		return next
	}
	out := c
	if next.FirstAt.Before(out.FirstAt) {
		out.FirstAt = next.FirstAt
	}
	if next.FirstAt.After(c.LastAt) {
		if c.Count > math.MaxInt64-next.Count {
			out.Count = math.MaxInt64
		} else {
			out.Count += next.Count
		}
	} else {
		out.Count = max(out.Count, next.Count)
	}
	if next.LastAt.After(out.LastAt) {
		out.LastAt = next.LastAt
	}
	return out
}

func (c MetricCoverage) Through(finish time.Time, minimum int64) bool {
	return c.Count >= minimum && !finish.IsZero() && !c.LastAt.Before(finish)
}
