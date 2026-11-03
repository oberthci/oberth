package service

import (
	"context"
	"time"

	"github.com/oberthci/oberth/internal/measured"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// An adopted grant uses its original running elapsed time, not server uptime.
// Unknown history remains conservative until actual release.
func (s *Scheduler) restoreBackfillHistory(ctx context.Context, run model.Run, r *admissionReservation) {
	estimate := s.measuredBackfillDuration(ctx, run)
	s.admission.mu.Lock()
	defer s.admission.mu.Unlock()
	r.estimate = estimate
	if run.StartedAt != nil {
		r.grantedAt = *run.StartedAt
	}
}

// Use the same bounded, retained step observations as run_resources. A sample
// is a whole workflow span, never a sum of parallel step durations. Classes
// separate release, promotion, schedule and branch workflows. Missing terminal
// timestamps and truncated history cannot supply a backfill prediction.
func (s *Scheduler) measuredBackfillDuration(ctx context.Context, run model.Run) time.Duration {
	reader, ok := s.store.(RunResourceReader)
	if !ok {
		return 0
	}
	history, err := reader.RunResources(ctx, run.RepoID, store.RunResourceFilter{Ref: "any", Runs: measured.MaxRuns, DurationClass: durationClass(run)})
	if err != nil || history.Truncated {
		return 0
	}
	type span struct {
		start, end time.Time
		incomplete bool
	}
	spans := make(map[string]span)
	for _, observation := range history.Observations {
		u := observation.Usage
		v := spans[u.RunID]
		if !u.Terminal || u.ContainerStartedAt.IsZero() || !u.ContainerFinishedAt.After(u.ContainerStartedAt) {
			v.incomplete = true
		} else {
			if v.start.IsZero() || u.ContainerStartedAt.Before(v.start) {
				v.start = u.ContainerStartedAt
			}
			if u.ContainerFinishedAt.After(v.end) {
				v.end = u.ContainerFinishedAt
			}
		}
		spans[u.RunID] = v
	}
	var samples []int64
	for _, previous := range history.Runs {
		if previous.Status != model.RunPassed || durationClass(previous) != durationClass(run) {
			continue
		}
		v := spans[previous.ID]
		if v.incomplete || v.start.IsZero() || !v.end.After(v.start) {
			continue
		}
		// Include known workflow startup/teardown outside the captured steps.
		if previous.StartedAt != nil && previous.StartedAt.Before(v.start) {
			v.start = *previous.StartedAt
		}
		if previous.FinishedAt != nil && previous.FinishedAt.After(v.end) {
			v.end = *previous.FinishedAt
		}
		samples = append(samples, int64(v.end.Sub(v.start)))
	}
	if len(samples) < measured.MinimumSamples {
		return 0
	}
	quantiles := model.Quantiles(samples)
	if quantiles.P95 == nil {
		return 0
	}
	return time.Duration(*quantiles.P95)
}
