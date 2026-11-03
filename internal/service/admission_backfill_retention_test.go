package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/measured"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func durableBackfillHistory(observed int) store.RunResourceHistory {
	at := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	history := store.RunResourceHistory{}
	for i := range measured.DefaultRuns {
		start := at.Add(-time.Duration(i) * 24 * time.Hour)
		duration := time.Duration(27+i) * time.Minute
		if i == measured.DefaultRuns-1 {
			duration = time.Hour
		}
		finish := start.Add(duration)
		id := fmt.Sprint(i)
		history.Runs = append(history.Runs, model.Run{ID: id, RepoID: 7, Release: true,
			Status: model.RunPassed, StartedAt: &start, FinishedAt: &finish})
		if i < observed {
			history.Observations = append(history.Observations, measured.Observation{Usage: model.StepResourceUsage{
				RunID: id, Terminal: true, ContainerStartedAt: start.Add(time.Minute), ContainerFinishedAt: finish.Add(-time.Minute),
			}})
		}
	}
	return history
}

func TestEASYHistorySurvivesPrunedObservations(t *testing.T) {
	for observed := 0; observed < measured.MinimumSamples; observed++ {
		t.Run(fmt.Sprint(observed), func(t *testing.T) {
			s := &Scheduler{store: easyHistoryStore{history: durableBackfillHistory(observed)}}
			if got := s.measuredBackfillDuration(t.Context(), model.Run{RepoID: 7, Release: true}); got != 55*time.Minute {
				t.Fatalf("30 durable runs, %d observed: p95 = %s, want 55m", observed, got)
			}
		})
	}
}

func TestEASYDurableHistoryRequiresCompletePassedClassSamples(t *testing.T) {
	before := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		change func(*model.Run)
	}{
		{"missing start", func(r *model.Run) { r.StartedAt = nil }},
		{"missing finish", func(r *model.Run) { r.FinishedAt = nil }},
		{"equal timestamps", func(r *model.Run) { r.FinishedAt = r.StartedAt }},
		{"reversed timestamps", func(r *model.Run) { r.FinishedAt = &before }},
		{"failed", func(r *model.Run) { r.Status = model.RunFailed }},
		{"other class", func(r *model.Run) { r.Release = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			history := durableBackfillHistory(0)
			history.Runs = history.Runs[:measured.MinimumSamples]
			// Four valid runs and one unusable row must remain unmeasured.
			tc.change(&history.Runs[0])
			s := &Scheduler{store: easyHistoryStore{history: history}}
			if got := s.measuredBackfillDuration(t.Context(), model.Run{RepoID: 7, Release: true}); got != 0 {
				t.Fatalf("ineligible durable row supplied p95 %s", got)
			}
		})
	}
}

func TestEASYDurableHistoryDoesNotMaskIncompleteObservations(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		t.Run(fmt.Sprint(truncated), func(t *testing.T) {
			history := durableBackfillHistory(measured.MinimumSamples)
			history.Runs = history.Runs[:measured.MinimumSamples]
			history.Truncated = truncated
			if !truncated {
				// Four complete spans plus an incomplete observed run cannot
				// be rescued by that run's otherwise valid durable timestamps.
				history.Observations[0].Usage.Terminal = false
			}
			s := &Scheduler{store: easyHistoryStore{history: history}}
			if got := s.measuredBackfillDuration(t.Context(), model.Run{RepoID: 7, Release: true}); got != 0 {
				t.Fatalf("unusable observed history supplied p95 %s", got)
			}
		})
	}
}

func TestEASYDurableReleaseForecastBackfillsSnapshot(t *testing.T) {
	// Use the supplied queue snapshot's release start and observation time.
	// The successful-run population models the review's approximately 55m p95;
	// the snapshot itself contains no passed release history.
	clock := &fakeClock{now: time.Date(2026, 11, 3, 1, 19, 41, 277573372, time.UTC)}
	g := backfillGate(clock)
	g.resources.CPUMilli, g.resources.MemoryBytes = 11000, 48<<30
	s := &Scheduler{admission: g, store: easyHistoryStore{history: durableBackfillHistory(3)}}
	started := clock.now
	run := model.Run{RepoID: 7, Release: true, StartedAt: &started}
	running := g.reserve(1)
	acquireRequestWithinBudget(t, running, easyRequest("release", 6300, 0))
	t.Cleanup(running.release)
	s.restoreBackfillHistory(t.Context(), run, running)
	clock.now = time.Date(2026, 11, 3, 1, 30, 37, 35950195, time.UTC)
	wantRemaining := 44*time.Minute + 4*time.Second + 241623177*time.Nanosecond
	if finish, ok := expectedFinish(running, clock.now); !ok || finish.Sub(clock.now) != wantRemaining {
		t.Fatalf("release forecast = %s, known=%v; want %s remaining (about 40m)", finish.Sub(clock.now), ok, wantRemaining)
	}
	head := pendingPriority(t, g, 2, model.PriorityHighest, easyRequest("head", 8200, 0))
	t.Cleanup(head.cancel)
	candidate := pending(t, g, 3, easyRequest("short", 4000, 5*time.Minute))
	t.Cleanup(candidate.cancel)
	if granted(g, head) || !granted(g, candidate) {
		t.Fatal("durable release forecast did not admit the safe short backfill")
	}
	if got := s.AdmissionSnapshot(candidate.sequence).BackfillMessage; got != "backfilled: measured finish within remaining head-wait window" {
		t.Fatalf("backfill message = %q", got)
	}
}
