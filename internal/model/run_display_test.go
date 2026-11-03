package model

import (
	"slices"
	"testing"
	"time"
)

// Every raw status, phase and projection a run can carry maps to exactly one
// of the seven user-facing statuses; "waiting", "infra-pending" and
// "deadline-exceeded" are reasons, never statuses.
func TestEveryRawPhaseMapsToExactlyOneDisplayStatus(t *testing.T) {
	started := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	statuses := []RunStatus{RunQueued, RunWaiting, RunRunning, RunPassed, RunFailed, RunInterrupted}
	phases := []string{"", "waiting", "preparing", "preflight", "running", "infrastructure-pending", "infrastructure", "job", "collect", "promotion", "publishing", "passed", "interrupted", PhaseCancelled}
	seen := map[string]int{}
	for _, status := range statuses {
		for _, phase := range phases {
			for _, startedAt := range []*time.Time{nil, &started} {
				for _, superseded := range []string{"", "newer"} {
					for _, retried := range []string{"", "retry"} {
						run := Run{Status: status, Phase: phase, StartedAt: startedAt, SupersededBy: superseded, AutoRetriedBy: retried}
						got := DisplayRunStatus(run)
						if !slices.Contains(DisplayStatuses, got) {
							t.Fatalf("%+v displays %q, outside the vocabulary %v", run, got, DisplayStatuses)
						}
						seen[got]++
					}
				}
			}
		}
	}
	for _, status := range DisplayStatuses {
		if seen[status] == 0 {
			t.Fatalf("no raw state displays %q", status)
		}
	}
}

func TestDisplayRunStatusCollapsesNotStartedRunsIntoQueued(t *testing.T) {
	started := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		run  Run
		want string
	}{
		{"durable queued", Run{Status: RunQueued}, DisplayQueued},
		{"wire waiting projection", Run{Status: RunWaiting}, DisplayQueued},
		{"claimed, preparing", Run{Status: RunRunning, Phase: "preparing"}, DisplayQueued},
		{"claimed, waiting for admission", Run{Status: RunRunning, Phase: "waiting"}, DisplayQueued},
		{"granted, admission not yet recorded", Run{Status: RunRunning, Phase: "waiting"}, DisplayQueued},
		{"preflight before admission recorded", Run{Status: RunRunning, Phase: "preflight"}, DisplayQueued},
		{"started", Run{Status: RunRunning, Phase: "running", StartedAt: &started}, DisplayRunning},
		{"uncertain workflow create", Run{Status: RunRunning, Phase: "infrastructure-pending", StartedAt: &started}, DisplayRunning},
		{"publishing", Run{Status: RunRunning, Phase: "publishing", StartedAt: &started}, DisplayRunning},
		{"passed", Run{Status: RunPassed, StartedAt: &started}, DisplayPassed},
		{"failed step", Run{Status: RunFailed, Phase: "job", StartedAt: &started}, DisplayFailed},
		{"failed by deadline", Run{Status: RunFailed, Phase: "job", Error: ExecutionDeadlineMessage, StartedAt: &started}, DisplayFailed},
		{"failed by memory", Run{Status: RunFailed, Phase: "job", Error: "OOMKilled", StartedAt: &started}, DisplayFailed},
		{"infra error retried", Run{Status: RunFailed, Phase: "infrastructure", AutoRetriedBy: "retry"}, DisplayInfraRetried},
		{"cancelled", Run{Status: RunInterrupted, Phase: PhaseCancelled}, DisplayCancelled},
		{"cancelled despite a successor", Run{Status: RunInterrupted, Phase: PhaseCancelled, SupersededBy: "newer"}, DisplayCancelled},
		{"superseded", Run{Status: RunInterrupted, Phase: "interrupted", SupersededBy: "newer"}, DisplaySuperseded},
		{"restart, retry needed", Run{Status: RunInterrupted, Phase: "interrupted"}, DisplayCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DisplayRunStatus(tc.run); got != tc.want {
				t.Fatalf("display status = %q, want %q", got, tc.want)
			}
			if tc.want == DisplayQueued && !tc.run.ClaimedUnstarted() && tc.run.Status == RunRunning {
				t.Fatal("a queued running run must be claimed and unstarted")
			}
		})
	}
}
