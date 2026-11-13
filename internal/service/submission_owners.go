package service

import (
	"context"
	"sync"

	"github.com/oberthci/oberth/internal/model"
)

// submissionRegistry cancels in-flight creates and detects conflicting adoption.
// It does not reserve execution capacity.
type submissionRegistry struct {
	mu     sync.Mutex
	owners map[int64]*submissionOwner
}
type submissionOwner struct {
	gate                    *submissionRegistry
	withdrawn               chan struct{}
	sequence, order, repoID int64
	runID                   string
	adopted                 bool
}

func newSubmissionRegistry() *submissionRegistry {
	return &submissionRegistry{owners: map[int64]*submissionOwner{}}
}
func (g *submissionRegistry) trackRun(run model.Run) *submissionOwner {
	r := &submissionOwner{withdrawn: make(chan struct{}), gate: g, sequence: run.QueueSequence, order: run.AdmissionSequence, repoID: run.RepoID, runID: run.ID}
	if r.order <= 0 {
		r.order = r.sequence
	}
	g.mu.Lock()
	g.owners[r.sequence] = r
	g.mu.Unlock()
	return r
}
func (r *submissionOwner) cancel() {
	g := r.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	g.withdrawLocked(r)
}
func (g *submissionRegistry) withdrawLocked(r *submissionOwner) {
	select {
	case <-r.withdrawn:
	default:
		close(r.withdrawn)
	}
	if g.owners[r.sequence] == r {
		delete(g.owners, r.sequence)
	}
}
func (g *submissionRegistry) abandon(sequence int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r := g.owners[sequence]; r != nil {
		g.withdrawLocked(r)
	}
}

// A durable cancellation wakes an in-flight create, including source seeding.
func (r *submissionOwner) creationContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		select {
		case <-r.withdrawn:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}
