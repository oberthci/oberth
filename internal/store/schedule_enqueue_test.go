package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/testtimeout"
)

func scheduleTestRequest(t *testing.T, s *Store, repo model.Repository, at time.Time) (model.ReceiveEventSpec, model.RunSpec, model.ScheduleFireSpec) {
	t.Helper()
	qualified, err := s.QualifiedRepoName(t.Context(), repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	spec := model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "main", SHA: strings.Repeat("a", 40), TestedSHA: strings.Repeat("a", 40), Actor: "oberth:scheduler", Trigger: "schedule", ScheduleEntry: "nightly"}
	event := model.ReceiveEventSpec{ID: "schedule-test", Actor: spec.Actor, RepoID: repo.ID, RefKind: spec.RefKind, Ref: spec.Ref, ObjectSHA: spec.SHA, CommitSHA: spec.SHA, Outcome: "queued"}
	return event, spec, model.ScheduleFireSpec{Repo: qualified, At: at}
}

func TestScheduleOccurrenceCoalescesOnlySameEntryAndBranch(t *testing.T) {
	for _, claimed := range []bool{false, true} {
		t.Run(fmt.Sprintf("claimed-%v", claimed), func(t *testing.T) {
			now := time.Date(2026, 10, 27, 1, 30, 0, 0, time.UTC)
			s := testStore(t, &now)
			repo := createRepo(t, s)
			event, spec, fire := scheduleTestRequest(t, s, repo, now)
			first, err := s.EnqueueScheduleOccurrence(t.Context(), event, spec, fire)
			if err != nil || first.Outcome != "fired" {
				t.Fatalf("first = %+v, %v", first, err)
			}
			if claimed {
				if _, err := s.ClaimNextRun(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			// Many terminal rows must not hide the active entry behind a page.
			for i := range 8 {
				other := spec
				other.Trigger, other.ScheduleEntry, other.Ref = "branch", "", fmt.Sprintf("feature-%d", i)
				run, err := s.EnqueueRun(t.Context(), other)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunFailed}); err != nil {
					t.Fatal(err)
				}
			}
			event.ID = "next-occurrence"
			event.ObjectSHA, event.CommitSHA = strings.Repeat("b", 40), strings.Repeat("b", 40)
			spec.SHA, spec.TestedSHA = event.ObjectSHA, event.CommitSHA
			fire.HasPrevious, fire.Previous, fire.At = true, now, now.Add(15*time.Minute)
			next, err := s.EnqueueScheduleOccurrence(t.Context(), event, spec, fire)
			if err != nil || next.Outcome != "coalesced" || next.Enqueued.ID != first.Enqueued.ID || len(next.Enqueued.Cancellations) != 0 {
				t.Fatalf("overlap = %+v, %v", next, err)
			}
			retained, err := s.Run(t.Context(), first.Enqueued.ID)
			if err != nil || !retained.Status.Active() || retained.SHA != strings.Repeat("a", 40) || retained.SupersededBy != "" {
				t.Fatalf("live run changed: %+v, %v", retained, err)
			}
			outcomes, err := s.ScheduleOutcomes(t.Context(), fire.Repo)
			if err != nil || len(outcomes) != 1 || outcomes[0].Outcome != "coalesced" {
				t.Fatalf("coalescing invisible: %+v, %v", outcomes, err)
			}
			cursors, err := s.ScheduleFires(t.Context(), fire.Repo)
			if err != nil || !cursors["nightly"].Equal(fire.At) {
				t.Fatalf("coalescing did not consume occurrence: %+v, %v", cursors, err)
			}
			for _, differentBranch := range []bool{false, true} {
				other := spec
				otherFire := fire
				otherFire.At = fire.At.Add(15 * time.Minute)
				otherFire.Previous = fire.At
				if differentBranch {
					other.Ref = "stable"
				} else {
					other.ScheduleEntry = "weekly"
					otherFire.HasPrevious, otherFire.Previous = false, time.Time{}
				}
				otherEvent := event
				otherEvent.ID, otherEvent.Ref = fmt.Sprintf("independent-%v", differentBranch), other.Ref
				result, err := s.EnqueueScheduleOccurrence(t.Context(), otherEvent, other, otherFire)
				if err != nil || result.Outcome != "fired" || result.Enqueued.ID == first.Enqueued.ID {
					t.Fatalf("independent entry/branch suppressed: %+v, %v", result, err)
				}
			}
		})
	}
}

func TestScheduleOccurrenceCursorFailureRollsBackRunReceiveAndAudit(t *testing.T) {
	now := time.Date(2026, 10, 27, 1, 30, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	event, spec, fire := scheduleTestRequest(t, s, repo, now)
	head, err := s.VerifyAuditChain(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TEMP TRIGGER fail_cursor BEFORE INSERT ON schedule_fires BEGIN SELECT RAISE(ABORT, 'injected cursor failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueScheduleOccurrence(t.Context(), event, spec, fire); err == nil || !strings.Contains(err.Error(), "injected cursor failure") {
		t.Fatalf("cursor failure not returned: %v", err)
	}
	for _, table := range []string{"runs", "receive_events", "schedule_fires"} {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial %s commit: %d, %v", table, count, err)
		}
	}
	after, err := s.VerifyAuditChain(t.Context())
	if err != nil || after.ID != head.ID || string(after.SHA256) != string(head.SHA256) {
		t.Fatalf("audit escaped rollback: %+v vs %+v, %v", after, head, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_cursor`); err != nil {
		t.Fatal(err)
	}
	if result, err := s.EnqueueScheduleOccurrence(t.Context(), event, spec, fire); err != nil || result.Outcome != "fired" {
		t.Fatalf("rolled back occurrence not retryable: %+v, %v", result, err)
	}
}

func TestScheduleOccurrenceDedupeReadFailureLeavesCursorPending(t *testing.T) {
	now := time.Date(2026, 10, 27, 1, 30, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	event, spec, fire := scheduleTestRequest(t, s, repo, now)
	if _, err := s.db.Exec(`ALTER TABLE runs RENAME TO unavailable_runs`); err != nil {
		t.Fatal(err)
	}
	_, enqueueErr := s.EnqueueScheduleOccurrence(t.Context(), event, spec, fire)
	if _, err := s.db.Exec(`ALTER TABLE unavailable_runs RENAME TO runs`); err != nil {
		t.Fatal(err)
	}
	if enqueueErr == nil || !strings.Contains(enqueueErr.Error(), "no such table: runs") {
		t.Fatalf("active query error not returned: %v", enqueueErr)
	}
	fires, err := s.ScheduleFires(t.Context(), fire.Repo)
	if err != nil || len(fires) != 0 {
		t.Fatalf("failed read consumed cursor: %+v, %v", fires, err)
	}
	if result, err := s.EnqueueScheduleOccurrence(t.Context(), event, spec, fire); err != nil || result.Outcome != "fired" {
		t.Fatalf("failed query was not retryable: %+v, %v", result, err)
	}
}

func TestScheduleOccurrenceCommitSurvivesRestartAndRejectsStaleSnapshot(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "schedule.db")
	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	repo := createRepo(t, s)
	at := time.Date(2026, 10, 27, 1, 30, 0, 0, time.UTC)
	event, spec, fire := scheduleTestRequest(t, s, repo, at)
	first, err := s.EnqueueScheduleOccurrence(ctx, event, spec, fire)
	if err != nil {
		t.Fatal(err)
	}
	// No app-side RecordScheduleFire: emulate loss of the enqueue reply.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishRun(ctx, first.Enqueued.ID, model.RunResult{Status: model.RunFailed}); err != nil {
		t.Fatal(err)
	}
	event.ID = "stale-retry-after-completion"
	fire.At = at.Add(time.Minute)
	result, err := s.EnqueueScheduleOccurrence(ctx, event, spec, fire)
	if err != nil || result.Outcome != "stale" {
		t.Fatalf("stale snapshot created another run: %+v, %v", result, err)
	}
	runs, err := s.ListRecentRuns(ctx, model.RunListFilter{RepoID: repo.ID})
	if err != nil || len(runs) != 1 {
		t.Fatalf("duplicate after restart/completion: %+v, %v", runs, err)
	}
	fires, err := s.ScheduleFires(ctx, fire.Repo)
	if err != nil || !fires[spec.ScheduleEntry].Equal(at) {
		t.Fatalf("enqueue committed without cursor: %+v, %v", fires, err)
	}
}

func TestScheduleOccurrenceConcurrentTransactionsConsumeSnapshotOnce(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "schedule.db")
	first, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	repo := createRepo(t, first)
	second, err := OpenAdminClient(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	event, spec, fire := scheduleTestRequest(t, first, repo, time.Now().UTC())
	start := make(chan struct{})
	results := make(chan string, 2)
	for i, database := range []*Store{first, second} {
		go func() {
			<-start
			request := event
			request.ID = fmt.Sprintf("concurrent-%d", i)
			result, err := database.EnqueueScheduleOccurrence(ctx, request, spec, fire)
			if err != nil {
				results <- err.Error()
				return
			}
			results <- result.Outcome
		}()
	}
	close(start)
	outcomes := map[string]int{}
	for range 2 {
		select {
		case outcome := <-results:
			outcomes[outcome]++
		case <-time.After(testtimeout.Start(15 * time.Second)):
			t.Fatal("concurrent enqueue stalled")
		}
	}
	if outcomes["fired"] != 1 || outcomes["stale"] != 1 {
		t.Fatalf("snapshot consumed more than once: %v", outcomes)
	}
}

func TestScheduleOccurrenceCoalescesRegardlessOfStoredTrigger(t *testing.T) {
	now := time.Date(2026, 10, 27, 1, 30, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	event, spec, fire := scheduleTestRequest(t, s, repo, now)
	active, err := s.EnqueueRun(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	// The current CHECK requires a named entry to have trigger=schedule.
	// Inject a legacy/future trigger in this disposable DB to prove dedupe
	// protects any same-entry run that enqueueRunTx could supersede.
	if _, err := s.db.Exec(`PRAGMA ignore_check_constraints=ON`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE runs SET trigger='push' WHERE id=?`, active.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA ignore_check_constraints=OFF`); err != nil {
		t.Fatal(err)
	}
	result, err := s.EnqueueScheduleOccurrence(t.Context(), event, spec, fire)
	if err != nil || result.Outcome != "coalesced" || result.Enqueued.ID != active.ID {
		t.Fatalf("same-entry run with another trigger was not coalesced: %+v, %v", result, err)
	}
	retained, err := s.Run(t.Context(), active.ID)
	if err != nil || retained.Status != model.RunQueued || retained.SupersededBy != "" {
		t.Fatalf("same-entry run was superseded: %+v, %v", retained, err)
	}
}
