package resourcesampler

import (
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// One snapshot contributes at most one source timestamp per metric. Multiple
// memory metric names in that scrape are not independent polling coverage.
func sourceCoverage(c model.MetricCoverage, at time.Time) model.MetricCoverage {
	if c.Count == 0 {
		return model.MetricCoverage{Count: 1, FirstAt: at, LastAt: at}
	}
	if at.After(c.LastAt) {
		c.FirstAt, c.LastAt = at, at
	}
	return c
}
