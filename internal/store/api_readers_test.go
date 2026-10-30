package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/testtimeout"
)

// Exercise public reads against real uncommitted WAL pages, rather than merely
// occupying the writer connection. Readers must see the last committed state.
func TestAPIReadsWhileWriterTransactionHeld(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "rc/test", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetQueuePriorityPatterns(t.Context(), "admin", []string{"rc/*"}); err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := s.reader().QueryRowContext(t.Context(), `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal mode=%q: %v", mode, err)
	}
	if s.db.Stats().MaxOpenConnections != 1 {
		t.Fatal("writer connection limit changed")
	}
	tx, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(t.Context(), `UPDATE repositories SET default_branch='uncommitted' WHERE id=?`, repo.ID); err != nil {
		t.Fatal(err)
	}
	// Parallel subtests cannot be used: their parent would release the writer
	// before they run. Run synchronously while the transaction remains held.
	checks := map[string]func(context.Context) error{
		"repositories": func(ctx context.Context) error {
			got, err := s.ListRepositories(ctx)
			if err == nil && (len(got) != 1 || got[0].DefaultBranch != "main") {
				return fmt.Errorf("unexpected committed repositories: %+v", got)
			}
			return err
		},
		"repository": func(ctx context.Context) error {
			got, err := s.Repository(ctx, repo.ID)
			if err == nil && got.DefaultBranch != "main" {
				return fmt.Errorf("read uncommitted branch %q", got.DefaultBranch)
			}
			return err
		},
		"repository-name": func(ctx context.Context) error { _, err := s.RepositoryByName(ctx, repo.Name); return err },
		"upstreams":       func(ctx context.Context) error { _, err := s.ListUpstreams(ctx); return err },
		"upstream":        func(ctx context.Context) error { _, err := s.Upstream(ctx, repo.UpstreamID); return err },
		"run":             func(ctx context.Context) error { _, err := s.Run(ctx, run.ID); return err },
		"runs":            func(ctx context.Context) error { _, err := s.ListRecentRuns(ctx, model.RunListFilter{}); return err },
		"counts":          func(ctx context.Context) error { _, err := s.CountRuns(ctx); return err },
		"latest":          func(ctx context.Context) error { _, err := s.ListLatestRunsPerRepo(ctx, 1); return err },
		"steps":           func(ctx context.Context) error { _, err := s.StepResults(ctx, run.ID); return err },
		"priority-patterns": func(ctx context.Context) error {
			got, err := s.QueuePriorityPatterns(ctx)
			if err == nil && (len(got) != 1 || got[0] != "rc/*") {
				return fmt.Errorf("unexpected priority patterns: %v", got)
			}
			return err
		},
		"next-queued": func(ctx context.Context) error {
			got, err := s.NextQueuedRun(ctx)
			if err == nil && got.ID != run.ID {
				return fmt.Errorf("next run=%s, want %s", got.ID, run.ID)
			}
			return err
		},
	}
	for name, check := range checks {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), testtimeout.Start(2*time.Second))
			defer cancel()
			if err := check(ctx); err != nil {
				t.Fatalf("read blocked behind writer or failed: %v", err)
			}
		})
	}
}
