package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestEvidencePurgeIntentSurvivesRemovalFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now()
	path := filepath.Join(t.TempDir(), "evidence.sqlite")
	s, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/purge", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishRun(ctx, run.ID, model.RunResult{Status: model.RunPassed}); err != nil {
		t.Fatal(err)
	}
	reader, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	diskErr := errors.New("partial removal")
	value, err := s.PurgeRunEvidence(ctx, "owner", run.ID, "both", "cleanup", 0, func() (int64, error) {
		// A separate connection must see a committed intent before any byte is
		// removed; rollback or a crash cannot erase the record of the attempt.
		var count int
		if err := reader.QueryRowContext(ctx, `SELECT count(*) FROM audit_actions WHERE resource_id=? AND action='run.purge.pending'`, run.ID).Scan(&count); err != nil || count != 1 {
			t.Fatalf("durable intent count=%d err=%v", count, err)
		}
		return 10, diskErr
	})
	if !errors.Is(err, diskErr) || value.State != "failed" || value.Bytes != 10 {
		t.Fatalf("purge=%#v err=%v", value, err)
	}
	markers, err := s.RunEvidencePurges(ctx, run.ID)
	if err != nil || len(markers) != 1 || markers[0].State != "failed" || markers[0].Error != "partial removal" {
		t.Fatalf("markers=%#v err=%v", markers, err)
	}
	if _, err := s.VerifyAuditChain(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestEvidencePurgeProtectsActiveRunsAndNewestTerminal(t *testing.T) {
	t.Parallel()
	for _, status := range []model.RunStatus{model.RunQueued, model.RunRunning} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			now := time.Now()
			s := testStore(t, &now)
			repo := createRepo(t, s)
			old, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/protected", strings.Repeat("a", 40)))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.ClaimNextRun(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := s.FinishRun(ctx, old.ID, model.RunResult{Status: model.RunPassed}); err != nil {
				t.Fatal(err)
			}
			active, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, old.Ref, strings.Repeat("b", 40)))
			if err != nil {
				t.Fatal(err)
			}
			if status == model.RunRunning {
				if _, err := s.ClaimNextRun(ctx); err != nil {
					t.Fatal(err)
				}
			}
			remove := func() (int64, error) { t.Fatal("removed protected evidence"); return 0, nil }
			// A claimed run waiting for admission is durably running. Neither it
			// nor a queued run counts as newer terminal evidence.
			if _, err := s.PurgeRunEvidence(ctx, "owner", active.ID, "both", "cleanup", 0, remove); !errors.Is(err, ErrEvidenceProtected) {
				t.Fatalf("active purge: %v", err)
			}
			if _, err := s.PurgeRunEvidence(ctx, "owner", old.ID, "both", "retention", 1, remove); !errors.Is(err, ErrEvidenceProtected) {
				t.Fatalf("newest terminal purge: %v", err)
			}
		})
	}
}

func TestEvidenceRemovalDoesNotBlockSchedulerWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "evidence.sqlite")
	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	repo := createRepo(t, s)
	old, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/old", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishRun(ctx, old.ID, model.RunResult{Status: model.RunPassed}); err != nil {
		t.Fatal(err)
	}
	queued, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/new", strings.Repeat("b", 40)))
	if err != nil {
		t.Fatal(err)
	}
	writer, err := OpenAdminClient(ctx, path, Options{BusyTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	value, err := s.PurgeRunEvidence(ctx, "owner", old.ID, "both", "cleanup", 0, func() (int64, error) {
		markers, err := writer.RunEvidencePurges(ctx, old.ID)
		if err != nil || len(markers) != 1 || markers[0].State != "pending" {
			t.Fatalf("removal is not fenced: %#v %v", markers, err)
		}
		// These writes must finish before removal returns. A write reservation
		// held across the callback fails at the independent connection's timeout.
		claimed, err := writer.ClaimNextRun(ctx)
		if err != nil || claimed.ID != queued.ID {
			t.Fatalf("claim during removal: %#v %v", claimed, err)
		}
		if _, err := writer.PutStepResult(ctx, model.StepResult{RunID: queued.ID, Burn: "test", Step: "unit", Status: model.StepPassed, DeclaredSize: "M"}); err != nil {
			t.Fatalf("step result during removal: %v", err)
		}
		return 10, nil
	})
	if err != nil || value.State != "complete" {
		t.Fatalf("purge=%#v err=%v", value, err)
	}
	if _, err := s.VerifyAuditChain(ctx); err != nil {
		t.Fatal(err)
	}
}
