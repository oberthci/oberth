package sourcedelivery

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
)

func TestSourceBudgetsDefaultAndCeilings(t *testing.T) {
	b, err := ResolveBudgets(nil)
	if err != nil || b != (Budgets{1, 2, 2, 32, 16}) {
		t.Fatal("default tuple", b, err)
	}
	for _, b := range []Budgets{{2, 4, 4, 32, 16}, {4, 8, 8, 128, 16}, {8, 32, 32, 128, 16}} {
		if _, err := ResolveBudgets(&b); err != nil {
			t.Fatal("valid tuple", b, err)
		}
	}
	for _, b := range []Budgets{{}, {0, 2, 2, 32, 16}, {1, 0, 2, 32, 16}, {1, 2, 0, 32, 16}, {1, 2, 2, 0, 16}, {1, 2, 2, 32, 0}, {9, 32, 32, 128, 16}, {1, 33, 2, 32, 16}, {1, 2, 33, 32, 16}, {1, 2, 2, 129, 16}, {1, 2, 2, 32, 17}, {2, 1, 2, 32, 16}, {2, 2, 1, 32, 16}} {
		if _, err := ResolveBudgets(&b); err == nil {
			t.Fatal("invalid tuple", b)
		}
	}
}

func TestConfiguredWorkflowWaitersRemainBoundedAndCancelable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := Budgets{Exports: 2, Archives: 4, Transfers: 3, Auth: 5, WorkflowWaiters: 2}
		s := New(Config{Budgets: &b})
		if cap(s.transfers) != b.Transfers || cap(s.auth) != b.Auth {
			t.Fatal("source tuple was not applied")
		}
		for range b.Transfers {
			s.transfers <- struct{}{}
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		caller := Caller{Namespace: "runs", Workflow: "work", UID: "uid"}
		var workers sync.WaitGroup
		for range 12 {
			workers.Go(func() {
				_, err := s.transferSlot(ctx, caller)
				if err == nil {
					t.Error("overflow got a transfer slot")
				}
			})
		}
		synctest.Wait()
		s.mu.Lock()
		waiting := s.waiters[caller]
		s.mu.Unlock()
		if waiting != b.WorkflowWaiters {
			t.Error("waiter bound ignored", waiting)
		}
		cancel()
		workers.Wait()
		if len(s.waiters) != 0 || len(s.transfers) != b.Transfers {
			t.Fatal("cancelled waiters leaked or stole slots")
		}
	})
}
