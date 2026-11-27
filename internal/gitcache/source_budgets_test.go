package gitcache

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/oberthci/oberth/internal/sourcedelivery"
)

func TestArchiveReservationsBoundConcurrentMisses(t *testing.T) {
	for _, exports := range []int{2, 4} {
		t.Run(fmt.Sprint(exports), func(t *testing.T) { synctest.Test(t, func(t *testing.T) { checkReservedMisses(t, exports) }) })
	}
}

func TestCacheAppliesTheValidatedSourceTuple(t *testing.T) {
	f := newExportWaitFixture(t)
	b := sourcedelivery.Budgets{Exports: 2, Archives: 4, Transfers: 4, Auth: 32, WorkflowWaiters: 16}
	c, err := New(t.Context(), Config{Root: t.TempDir(), Scratch: t.TempDir(), Binary: f.cache.config.Binary, SourceDelivery: &b})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	}()
	if cap(c.export) != b.Exports || c.staged.capacity != b.Archives {
		t.Fatal("cache ignored source tuple")
	}
}

func TestExportBuildersUseTheConfiguredPermits(t *testing.T) {
	for _, exports := range []int{2, 4} {
		t.Run(fmt.Sprint(exports), func(t *testing.T) { synctest.Test(t, func(t *testing.T) { checkExportBuilders(t, exports) }) })
	}
}

func checkExportBuilders(t *testing.T, exports int) {
	f := newExportWaitFixture(t)
	c := f.cache
	c.export = make(chan struct{}, exports)
	c.staged.capacity = 2 * exports
	if err := c.lock(t.Context(), f.path); err != nil {
		t.Fatal(err)
	}
	defer c.unlock(f.path)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var workers sync.WaitGroup
	for i := range 3 * exports {
		workers.Go(func() {
			b := f.binding
			b.Template = fmt.Sprintf("miss-%d", i)
			s, err := c.Stage(ctx, b)
			if err == nil {
				_ = s.Body.Close()
			}
		})
	}
	synctest.Wait()
	if len(c.export) != exports || len(c.staged.entries) != 2*exports {
		t.Error("configured export/archive bound ignored", len(c.export), len(c.staged.entries))
	}
	cancel()
	workers.Wait()
	c.staged.workers.Wait()
	if len(c.export) != 0 || len(c.staged.entries) != 0 {
		t.Fatal("cancelled export jobs leaked")
	}
}

func checkReservedMisses(t *testing.T, exports int) {
	f := newExportWaitFixture(t)
	c := f.cache
	c.export = make(chan struct{}, exports)
	c.staged.capacity = 2 * exports
	var held []sourcedelivery.Staged
	for i := 0; i < c.staged.capacity-1; i++ {
		b := f.binding
		b.Template = fmt.Sprintf("seed-%d", i)
		s, err := c.Stage(t.Context(), b)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, s)
	}
	defer func() {
		for _, s := range held {
			_ = s.Body.Close()
		}
	}()
	if err := c.lock(t.Context(), f.path); err != nil {
		t.Fatal(err)
	}
	unlock := sync.OnceFunc(func() { c.unlock(f.path) })
	defer unlock()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var workers sync.WaitGroup
	for i := range 12 {
		workers.Go(func() {
			b := f.binding
			b.Template = fmt.Sprintf("miss-%d", i)
			s, err := c.Stage(ctx, b)
			if err == nil {
				_ = s.Body.Close()
			}
		})
	}
	synctest.Wait()
	n, building := archiveCounts(c.staged)
	if n != c.staged.capacity || building != 1 || len(c.export) != 1 {
		t.Errorf("reservation overflow: entries=%d builds=%d exports=%d capacity=%d", n, building, len(c.export), c.staged.capacity)
	}
	cancel()
	workers.Wait()
	c.staged.workers.Wait()
	if len(c.export) != 0 || len(c.staged.entries) != len(held) {
		t.Fatal("cancelled reservations leaked", len(c.export), len(c.staged.entries))
	}
}

func TestArchiveJoinOwnsOneBuildAndIndependentCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newExportWaitFixture(t)
		c := f.cache
		var builds atomic.Int32
		c.config.Observe = func(s sourcedelivery.Summary) {
			if s.Kind == "job" && s.Leaders == 1 {
				builds.Add(1)
			}
		}
		if err := c.lock(t.Context(), f.path); err != nil {
			t.Fatal(err)
		}
		unlock := sync.OnceFunc(func() { c.unlock(f.path) })
		defer unlock()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		first, second := make(chan exportWaitResult, 1), make(chan exportWaitResult, 1)
		go func() { s, err := c.Stage(ctx, f.binding); first <- exportWaitResult{s, err} }()
		synctest.Wait()
		go func() { s, err := c.Stage(t.Context(), f.binding); second <- exportWaitResult{s, err} }()
		synctest.Wait()
		if len(c.export) != 1 || len(c.staged.entries) != 1 {
			t.Error("same binding started another build")
		}
		cancel()
		if r := <-first; r.err == nil {
			t.Error("cancelled waiter got a reader")
			_ = r.stage.Body.Close()
		}
		synctest.Wait()
		if len(c.export) != 1 {
			t.Fatal("one caller cancelled its peer build")
		}
		unlock()
		r := <-second
		if r.err != nil || !r.stage.Waited {
			t.Fatal("peer did not complete after join", r.err)
		}
		_ = r.stage.Body.Close()
		if builds.Load() != 1 {
			t.Fatal("same binding ran more than one physical build", builds.Load())
		}
		if len(c.staged.entries) != 1 {
			t.Fatal("joined build did not retain one archive")
		}
	})
}

func TestArchiveShutdownJoinsReservedBuilds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newExportWaitFixture(t)
		c := f.cache
		if err := c.lock(t.Context(), f.path); err != nil {
			t.Fatal(err)
		}
		defer c.unlock(f.path)
		done := make(chan error, 1)
		go func() {
			s, err := c.Stage(t.Context(), f.binding)
			if err == nil {
				_ = s.Body.Close()
			}
			done <- err
		}()
		synctest.Wait()
		c.staged.close()
		if err := <-done; err == nil {
			t.Fatal("shutdown completed a blocked load")
		}
		if len(c.export) != 0 || len(c.staged.entries) != 0 {
			t.Fatal("shutdown leaked permits or reservations")
		}
	})
}

func archiveCounts(s *stagedArchives) (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	building := 0
	for _, e := range s.entries {
		if e.building {
			building++
		}
	}
	return len(s.entries), building
}
