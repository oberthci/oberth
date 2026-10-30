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
		"pilot resources": func(ctx context.Context) error {
			_, err := s.PilotResources(ctx, run.ID)
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			return err
		},
		"pilot receipt": func(ctx context.Context) error {
			_, err := s.CompletedPilotReceipt(ctx, run.ID)
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			return err
		},
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
	ctx, cancel := context.WithTimeout(t.Context(), testtimeout.Start(20*time.Second))
	defer cancel()
	start := make(chan struct{})
	errs := make(chan error, writers+1)
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Go(func() {
			<-start
			for i := range rounds {
				if _, err := s.SetRunJobName(ctx, id, fmt.Sprintf("workflow-%d", i)); err != nil {
					errs <- err
					return
				}
				if _, err := s.PutStepResult(ctx, model.StepResult{RunID: id, Burn: "test", Step: fmt.Sprintf("step-%d", i), Ordinal: i, Status: model.StepPassed, DeclaredSize: "S"}); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	var maxLookup time.Duration
	wg.Go(func() {
		<-start
		for range rounds {
			// These are the store lookups used by receive and ls-remote cache
			// identity resolution, alongside the system-status run list.
			began := time.Now()
			lookup, stop := context.WithTimeout(ctx, testtimeout.Scale(time.Second))
			_, err := s.UpstreamByName(lookup, "codeberg")
			if err == nil {
				_, err = s.RepositoryByName(lookup, "codeberg/codeberg.org/oberth")
			}
			if err == nil {
				_, err = s.ListRecentRuns(lookup, model.RunListFilter{})
			}
			stop()
			maxLookup = max(maxLookup, time.Since(began))
			if err != nil {
				errs <- fmt.Errorf("lookup under %d writers: %w", writers, err)
				return
			}
		}
	})
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if maxLookup > testtimeout.Scale(time.Second) {
		t.Errorf("lookup latency %s exceeds budget %s", maxLookup, testtimeout.Scale(time.Second))
	}
	t.Logf("%d writers, %d status+step writes each; max lookup+list latency %s", writers, rounds, maxLookup)
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
