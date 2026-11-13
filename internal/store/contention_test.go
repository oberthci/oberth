package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/testtimeout"
)

func TestLiveReadsDoNotWaitForWriter(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run := projectionRun(t, s, repo.ID, "main", projectionSourceSHA)
	tx, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(t.Context(), `UPDATE repositories SET default_branch='uncommitted' WHERE id=?`, repo.ID); err != nil {
		t.Fatal(err)
	}
	checks := map[string]func(context.Context) error{
		"upstream": func(ctx context.Context) error { _, err := s.UpstreamByName(ctx, "codeberg"); return err },
		"repository": func(ctx context.Context) error {
			got, err := s.Repository(ctx, repo.ID)
			if err == nil && got.DefaultBranch != "main" {
				return fmt.Errorf("read uncommitted branch %q", got.DefaultBranch)
			}
			return err
		},
		"qualified mapping": func(ctx context.Context) error {
			_, err := s.RepositoryByName(ctx, "codeberg/codeberg.org/oberth")
			return err
		},
		"bare mapping":  func(ctx context.Context) error { _, err := s.RepositoryByName(ctx, "oberth"); return err },
		"flat run list": func(ctx context.Context) error { _, err := s.ListRecentRuns(ctx, model.RunListFilter{}); return err },
		"run":           func(ctx context.Context) error { _, err := s.Run(ctx, run.ID); return err },
		"summary":       func(ctx context.Context) error { _, err := s.Summary(ctx); return err },
		"idle":          func(ctx context.Context) error { _, err := s.IdleQueueStatus(ctx); return err },
	}
	for name, check := range checks {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), testtimeout.Scale(time.Second))
			defer cancel()
			if err := check(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	// A later committed change must be visible without any mapping cache.
	if _, err := s.SetRepositoryDefaultBranch(t.Context(), repo.ID, "next"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Repository(t.Context(), repo.ID)
	if err != nil || got.DefaultBranch != "next" {
		t.Fatalf("committed mapping: %+v, %v", got, err)
	}
}

func TestConcurrentStatusWritersAndReceiveLookups(t *testing.T) {
	const writers, rounds = 16, 32
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ids := make([]string, writers)
	for i := range ids {
		run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, fmt.Sprintf("load/%d", i), strings.Repeat("a", 40)))
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = run.ID
	}
	ctx := t.Context()
	for round := range rounds {
		// Reserve the only writer connection before releasing the workers.
		// Every lookup must finish with a real write still uncommitted; neither
		// fast workers nor a delayed scheduler can remove the contention.
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE repositories SET default_branch='uncommitted' WHERE id=?`, repo.ID); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		start := make(chan struct{})
		ready := make(chan struct{}, writers)
		errs := make(chan error, 2*writers+1)
		lookupStarted := make(chan struct{})
		// Each status commit requests a read before its worker may write the
		// step. This proves overlap without a reader spinning at a rate set by
		// host scheduling, starving the very writers it is meant to observe.
		lookupRequests := make(chan chan struct{})
		var wg sync.WaitGroup
		for _, id := range ids {
			wg.Go(func() {
				ready <- struct{}{}
				<-start
				<-lookupStarted
				if _, err := s.SetRunWorkflowName(ctx, id, fmt.Sprintf("workflow-%d", round)); err != nil {
					errs <- err
					return
				}
				observed := make(chan struct{})
				lookupRequests <- observed
				<-observed
				if _, err := s.PutStepResult(ctx, model.StepResult{NodeID: ("test") + "." + (fmt.Sprintf("step-%d", round)), RunID: id, Stage: "test", Step: fmt.Sprintf("step-%d", round), Ordinal: round, Status: model.StepPassed}); err != nil {
					errs <- err
				}
			})
		}
		for range writers {
			<-ready
		}
		if round == 0 {
			t.Log("concurrent workers ready")
		}
		close(start)
		// These are the receive/ls-remote identity lookups and status list.
		// TestLiveReadsDoNotWaitForWriter catches writer-pool routing errors
		// with a scaled bound. This generous bound diagnoses a deadlock here
		// without treating host scheduling latency as a correctness failure.
		heldCtx, stopHeld := context.WithTimeout(ctx, testtimeout.Scale(time.Minute))
		_, upstreamErr := s.UpstreamByName(heldCtx, "codeberg")
		lookup, repoErr := s.RepositoryByName(heldCtx, "codeberg/codeberg.org/oberth")
		runs, listErr := s.ListRecentRuns(heldCtx, model.RunListFilter{})
		stopHeld()
		rollbackErr := tx.Rollback()
		// Keep reading while the workers commit, append to the WAL and may
		// auto-checkpoint. No writer starts until the first lookup succeeds.
		writersDone := make(chan struct{})
		readerDone := make(chan struct{})
		concurrentLookups := 0
		go func() {
			defer close(readerDone)
			lookup := func() error {
				if _, err := s.UpstreamByName(ctx, "codeberg"); err != nil {
					return err
				}
				if _, err := s.RepositoryByName(ctx, "codeberg/codeberg.org/oberth"); err != nil {
					return err
				}
				_, err := s.ListRecentRuns(ctx, model.RunListFilter{})
				return err
			}
			firstErr := lookup()
			// Release workers on failure too, so all goroutines can be joined.
			close(lookupStarted)
			if firstErr != nil {
				errs <- fmt.Errorf("round %d first concurrent lookup: %w", round, firstErr)
			}
			for {
				select {
				case observed := <-lookupRequests:
					if err := lookup(); err != nil {
						errs <- fmt.Errorf("round %d lookup during commits: %w", round, err)
					} else {
						concurrentLookups++
					}
					// Release this worker on failure too, so teardown can join it.
					close(observed)
				case <-writersDone:
					return
				}
			}
		}()
		wg.Wait()
		close(writersDone)
		<-readerDone
		if concurrentLookups != writers {
			t.Errorf("round %d: lookups alongside commits = %d, want %d", round, concurrentLookups, writers)
		}
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		if err := errors.Join(upstreamErr, repoErr, listErr, rollbackErr); err != nil {
			t.Fatal(err)
		}
		if lookup.ID != repo.ID || lookup.DefaultBranch != "main" || len(runs) != writers {
			t.Fatalf("round %d: committed lookup=%+v, runs=%d", round, lookup, len(runs))
		}
		if t.Failed() {
			t.FailNow()
		}
	}
	// Preserve all 16 x 32 writes and verify that none was lost.
	for _, id := range ids {
		run, err := s.Run(ctx, id)
		if err != nil || run.WorkflowName != fmt.Sprintf("workflow-%d", rounds-1) {
			t.Fatalf("final workflow for %s: %+v, %v", id, run, err)
		}
		steps, err := s.StepResults(ctx, id)
		if err != nil || len(steps) != rounds {
			t.Fatalf("final steps for %s: count=%d, %v", id, len(steps), err)
		}
		for round, step := range steps {
			if step.Step != fmt.Sprintf("step-%d", round) || step.Ordinal != round || step.Status != model.StepPassed {
				t.Fatalf("final step for %s round %d: %+v", id, round, step)
			}
		}
	}
	t.Logf("%d writers, %d status+step writes each; lookups completed with writer held and alongside commits in every round", writers, rounds)
}

func TestLiveReaderPoolIsReadOnlyAndBounded(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	var release []func()
	defer func() {
		for _, close := range release {
			close()
		}
	}()
	for range liveReadConnections {
		conn, err := s.readDB.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		release = append(release, func() { _ = conn.Close() })
		var queryOnly int
		if err := conn.QueryRowContext(t.Context(), `PRAGMA query_only`).Scan(&queryOnly); err != nil || queryOnly != 1 {
			t.Fatalf("query_only=%d, %v", queryOnly, err)
		}
		if _, err := conn.ExecContext(t.Context(), `DELETE FROM runs`); err == nil {
			t.Fatal("reader accepted a write")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.ListUpstreams(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exhausted reader pool: %v", err)
	}
	// Reader saturation cannot consume the writer connection.
	if _, err := s.db.ExecContext(t.Context(), `UPDATE repositories SET updated_at=updated_at`); err != nil {
		t.Fatal(err)
	}
}
