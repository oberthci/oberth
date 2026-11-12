package service

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type slowCPUReader struct {
	liveCPUFixture
	mu              sync.Mutex
	active, maximum int
	entered         chan string
}

func (j *slowCPUReader) LivePodRequests(ctx context.Context, run model.Run) (*argoworkflow.PodRequests, error) {
	j.mu.Lock()
	j.active++
	j.maximum = max(j.maximum, j.active)
	j.mu.Unlock()
	defer func() { j.mu.Lock(); j.active--; j.mu.Unlock() }()
	j.entered <- run.ID
	<-ctx.Done()
	return nil, ctx.Err()
}
func TestBackgroundCPURefreshBoundsWorkWithoutBlockingDispatch(t *testing.T) {
	f := newGroupFixture(t, 0)
	s := f.scheduler
	g := s.admission
	g.cpuMilli = 9600
	j := &slowCPUReader{liveCPUFixture: liveCPUFixture{JobController: f.engine, pool: argoworkflow.CPUAdmission{Pool: "local", Milli: 9600}}, entered: make(chan string, 64)}
	s.jobs = j
	for i := 0; i < 16; i++ {
		old := f.branch(t, f.exampleService, fmt.Sprintf("owned-%d", i), "a")
		claimed, err := f.store.ClaimNextRun(t.Context())
		if err != nil || claimed.ID != old.ID {
			t.Fatal(claimed, err)
		}
		r, err := g.adopt(claimed)
		if err != nil {
			t.Fatal(err)
		}
		s.initializeAdoptedCPU(t.Context(), claimed, r)
		if g.credit(r, g.pool(r)) != 9600 {
			t.Fatal("startup did not keep conservative credit")
		}
	}
	if len(j.entered) != 0 {
		t.Fatal("startup performed serial live reads")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { s.refreshCPUReservations(ctx); close(done) }()
	for range 2 {
		select {
		case <-j.entered:
		case <-time.After(testTimeout(time.Second)):
			t.Fatal("background workers did not start")
		}
	}
	// The real dispatch claim must remain responsive while both API reads block.
	queued := f.branch(t, f.terraform, "independent", "b")
	claimCtx, stop := context.WithTimeout(t.Context(), testTimeout(time.Second))
	defer stop()
	claimed, err := s.claimDispatchRun(claimCtx, 16)
	if err != nil || claimed.ID != queued.ID {
		t.Fatal("CPU observation blocked dispatch", claimed, err)
	}
	select {
	case <-done:
	case <-time.After(testTimeout(7 * time.Second)):
		t.Fatal("refresh exceeded total cycle budget")
	}
	j.mu.Lock()
	maximum, active := j.maximum, j.active
	j.mu.Unlock()
	if maximum != 2 || active != 0 {
		t.Fatal("unbounded or leaked refresh workers", maximum, active)
	}
	if len(j.entered) > 4 {
		t.Fatal("cycle attempted too many two-second reads", len(j.entered)+2)
	}
	g.mu.Lock()
	after := g.lastCPURefresh
	g.mu.Unlock()
	for len(j.entered) > 0 {
		<-j.entered
	}
	nextCtx, nextStop := context.WithTimeout(t.Context(), testTimeout(time.Second))
	defer nextStop()
	s.refreshCPUReservations(nextCtx)
	select {
	case id := <-j.entered:
		run, err := f.store.Run(t.Context(), id)
		if err != nil || run.QueueSequence <= after {
			t.Fatal("refresh rotation starved survivors", run, after, err)
		}
	default:
		t.Fatal("next cycle did not refresh survivors")
	}
}
