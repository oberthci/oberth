package gitcache

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestRepositoryRemovalDoesNotMutateCatalogBeforeReadLock(t *testing.T) {
	r := newTestRepository(t)
	c := newTestCache(t, r.upstream)
	ready, err := c.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	lock := c.repoLock(ready.Path)
	if err = lock.RLockContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer lock.RUnlock()
	called := false
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	err = c.RemoveRepositoryWith(ctx, "example", func() error { called = true; return nil })
	if !errors.Is(err, context.DeadlineExceeded) || called {
		t.Fatalf("catalog mutation escaped lock: %v called=%t", err, called)
	}
	if _, err = os.Stat(ready.Path); err != nil {
		t.Fatal("blocked removal deleted cache", err)
	}
}
func TestRepositoryRemovalPreservesPendingReceiveEvidence(t *testing.T) {
	r := newTestRepository(t)
	c := newTestCache(t, r.upstream)
	ready, err := c.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.reserveReceiveLocked(t.Context(), "example", ready.Path, "actor", ReleaseAdmission{}); err != nil {
		t.Fatal(err)
	}
	called := false
	err = c.RemoveRepositoryWith(t.Context(), "example", func() error { called = true; return nil })
	if !errors.Is(err, ErrArchivePendingReceive) || called {
		t.Fatalf("pending evidence lost: %v called=%t", err, called)
	}
	if _, err = os.Stat(ready.Path); err != nil {
		t.Fatal(err)
	}
}
