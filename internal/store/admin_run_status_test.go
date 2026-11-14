package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestAdminRunStatusUsesDisplayValuesBeforeBounds(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	fixtures := []struct {
		status                                   model.RunStatus
		phase, reason, failure, superseded, want string
		started                                  bool
	}{
		{status: model.RunQueued, want: "queued"},
		{status: model.RunRunning, want: "queued"},
		{status: model.RunRunning, started: true, want: "running"},
		{status: model.RunRunning, phase: "publishing", want: "running"},
		{status: model.RunRunning, phase: "infrastructure-pending", want: "queued"},
		{status: model.RunRunning, failure: WorkflowSpecMismatchPendingMessage, want: "queued"},
		{status: model.RunRunning, reason: PolicyRefreshWaitingReason, want: "queued"},
		{status: model.RunRunning, phase: "infrastructure-pending", reason: "kept; " + PolicyRefreshWaitingReason, want: "queued"},
		{status: model.RunPassed, want: "passed"},
		{status: model.RunFailed, want: "failed"},
		{status: model.RunFailed, failure: "DeadlineExceeded", want: "failed"},
		{status: model.RunFailed, failure: "observe adopted Workflow job: " + model.ExecutionDeadlineMessage + " Server watchdog expired: context deadline exceeded\ncleanup failure", want: "failed"},
		{status: model.RunFailed, failure: "DeadlineExceeded", reason: "historical automatic retry " + strings.Repeat("f", 32), want: "failed"},
		{status: model.RunInterrupted, phase: model.PhaseCancelled, superseded: strings.Repeat("f", 32), want: "cancelled"},
		{status: model.RunInterrupted, superseded: strings.Repeat("f", 32), want: "cancelled"},
		{status: model.RunInterrupted, want: "cancelled"},
	}
	want := map[string]map[string]bool{}
	for i, fixture := range fixtures {
		run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, fmt.Sprintf("feature/status-%d", i), strings.Repeat("a", 40)))
		if err != nil {
			t.Fatal(err)
		}
		var started any
		if fixture.started {
			started = unixNano(now)
		}
		if _, err = s.db.Exec(`UPDATE runs SET status=?,phase=?,reason=?,error=?,superseded_by=?,started_at=?,finished_at=? WHERE id=?`, fixture.status, fixture.phase, fixture.reason, fixture.failure, fixture.superseded, started, unixNano(now), run.ID); err != nil {
			t.Fatal(err)
		}
		if want[fixture.want] == nil {
			want[fixture.want] = map[string]bool{}
		}
		want[fixture.want][run.ID] = true
	}
	for status, ids := range want {
		for _, latest := range []bool{false, true} {
			rows, err := s.ListRecentRuns(t.Context(), model.RunListFilter{Status: model.RunStatusFilter{status}, Limit: 100, LatestPerRef: latest})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != len(ids) {
				t.Fatalf("status=%s latest=%t got %d want %d", status, latest, len(rows), len(ids))
			}
			for _, row := range rows {
				if !ids[row.ID] || model.DisplayRunStatus(row) != status {
					t.Fatalf("wrong status %s: %+v", status, row)
				}
			}
		}
	}
	hidden, err := s.ListRecentRuns(t.Context(), model.RunListFilter{Status: model.RunStatusFilter{"failed", "queued"}, RepoIDs: []int64{}})
	if err != nil || len(hidden) != 0 {
		t.Fatalf("filter leaked scope: %+v %v", hidden, err)
	}
}

func TestAdminRunStatusDoesNotResurrectEarlierRefAttempt(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	first, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "feature/same", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishRun(t.Context(), first.ID, model.RunResult{Status: model.RunFailed}); err != nil {
		t.Fatal(err)
	}
	next, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "feature/same", strings.Repeat("b", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishRun(t.Context(), next.ID, model.RunResult{Status: model.RunPassed}); err != nil {
		t.Fatal(err)
	}
	runs, err := s.ListRecentRuns(t.Context(), model.RunListFilter{Status: model.RunStatusFilter{"failed"}, LatestPerRef: true})
	if err != nil || len(runs) != 0 {
		t.Fatalf("filtered out head resurrected history: %+v %v", runs, err)
	}
}

func TestAdminRunStatusRejectsReasonsOutsideCandidateVocabulary(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	for _, status := range []string{"waiting", "infra-pending", "deadline-exceeded", "infra-retried"} {
		if _, err := s.ListRecentRuns(t.Context(), model.RunListFilter{Status: model.RunStatusFilter{status}}); err == nil {
			t.Fatalf("reason %q accepted as a display status", status)
		}
	}
}
