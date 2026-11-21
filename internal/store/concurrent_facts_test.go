package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func pairedStores(t *testing.T) []*Store {
	t.Helper()
	first := testStore(t)
	second, err := Open(t.Context(), first.path, first.options)
	requireOK(t, err)
	t.Cleanup(func() { requireOK(t, second.Close()) })
	return []*Store{first, second}
}

func contestFact(t *testing.T, stores []*Store, write func(*Store) error) {
	t.Helper()
	start := make(chan struct{})
	results := make(chan error, 32)
	var workers sync.WaitGroup
	for i := range 32 {
		workers.Go(func() { <-start; results <- write(stores[i%2]) })
	}
	close(start)
	workers.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else {
			requireCode(t, err, publicerr.Conflict)
		}
	}
	if winners != 1 {
		t.Fatal("compare-and-set winners", winners)
	}
}

func TestSecondCASMethodsAcrossHandles(t *testing.T) {
	t.Parallel()
	stores := pairedStores(t)
	s := stores[0]
	r, _ := accepted(t, s, "cas-run")
	a := stateport.Action{Actor: "operator"}
	contestFact(t, stores, func(db *Store) error {
		_, err := db.Cancel(t.Context(), a, stateport.RunCancellation{ID: r.ID, Revision: r.Revision})
		return err
	})
	r, err := s.Run(t.Context(), r.ID)
	requireOK(t, err)
	contestFact(t, stores, func(db *Store) error {
		return db.Finish(t.Context(), a, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunPassed})
	})
	p := publicationIntent(r)
	requireOK(t, s.PreparePublication(t.Context(), a, p))
	contestFact(t, stores, func(db *Store) error {
		_, err := db.StartPublicationAttempt(t.Context(), a, stateport.PublicationAttempt{ID: p.Publication.ID})
		return err
	})
	contestFact(t, stores, func(db *Store) error {
		return db.SettlePublication(t.Context(), a, stateport.PublicationResult{ID: p.Publication.ID, Attempt: 1, Delivered: true})
	})
	_, _, err = s.Audit(t.Context(), 0, 200)
	requireOK(t, err)
}

// The review's reader/writer stress case uses fewer iterations here so the
// deterministic regression remains suitable for the normal race gate.
func TestSecondParallelReadersWriters(t *testing.T) {
	t.Parallel()
	stores := pairedStores(t)
	s := stores[0]
	a := stateport.Action{Actor: "operator"}
	issue, err := s.CreateIssue(t.Context(), a, model.Issue{Kind: model.IssueManual, Title: "counter", Body: "0"})
	requireOK(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	results := make(chan error, 16)
	var workers sync.WaitGroup
	for worker := range 8 {
		workers.Go(func() { results <- incrementIssue(ctx, stores[worker%2], issue.ID, worker) })
	}
	for reader := range 8 {
		workers.Go(func() {
			for range 16 {
				if _, err := stores[reader%2].Issue(ctx, issue.ID); err != nil {
					results <- err
					return
				}
			}
			results <- nil
		})
	}
	workers.Wait()
	close(results)
	for err := range results {
		requireOK(t, err)
	}
	current, err := s.Issue(t.Context(), issue.ID)
	requireOK(t, err)
	if current.Body != "64" || current.Revision != 65 {
		t.Fatal("lost counter updates", current)
	}
	page, err := s.Events(t.Context(), stateport.EventFilter{Limit: 200})
	requireOK(t, err)
	if len(page.Events) != 64 {
		t.Fatal("lost atomic events", len(page.Events))
	}
	_, head, err := s.Audit(t.Context(), 0, 200)
	requireOK(t, err)
	if head.ID != 65 {
		t.Fatal("lost audit", head)
	}
}

func incrementIssue(ctx context.Context, s *Store, id int64, worker int) error {
	for i := range 8 {
		for {
			current, err := s.Issue(ctx, id)
			if err != nil {
				return err
			}
			count, err := strconv.Atoi(current.Body)
			if err != nil {
				return err
			}
			event := atomicEvent(s, fmt.Sprintf("worker-%d-%d", worker, i))
			_, err = s.UpdateIssue(ctx, stateport.Action{Actor: "operator", Events: []stateport.EventWrite{event}}, model.IssuePatch{ID: id, ExpectedRevision: &current.Revision, Body: new(strconv.Itoa(count + 1))})
			if err == nil {
				break
			}
			if !conflictError(err) {
				return err
			}
		}
	}
	return nil
}

func conflictError(err error) bool {
	var public *publicerr.Error
	return errors.As(err, &public) && public.Code() == publicerr.Conflict
}

func TestSecondWriterBudgets(t *testing.T) {
	t.Parallel()
	stores := pairedStores(t)
	s, second := stores[0], stores[1]
	second.options.BusyTimeout = 80 * time.Millisecond
	held, err := s.writer.BeginTx(t.Context(), nil)
	requireOK(t, err)
	defer func() { requireOK(t, held.Rollback()) }()
	for _, deadline := range []time.Duration{0, 20 * time.Millisecond} {
		ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
		if deadline > 0 {
			var innerCancel context.CancelFunc
			ctx, innerCancel = context.WithTimeout(ctx, deadline)
			defer innerCancel()
		}
		_, err := second.CreateIssue(ctx, stateport.Action{Actor: "operator"}, model.Issue{Kind: model.IssueManual, Title: "blocked"})
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("writer budget cause lost", err)
		}
	}
}
