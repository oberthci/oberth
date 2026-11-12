package service

import (
	"context"
	"errors"
	"sync"

	"github.com/oberthci/oberth/internal/model"
)

var errSubmissionWithdrawn = errors.New("service: submission owner withdrawn: run is no longer active")

type submissionRegistry struct {
	mu     sync.Mutex
	owners map[int64]*submissionOwner
}
type submissionOwner struct {
	gate                                                                  *submissionRegistry
	withdrawn                                                             chan struct{}
	sequence, order, repoID                                               int64
	runID, repo, ref, sha                                                 string
	refKind                                                               model.RefKind
	prepared, granted, submitted, released, canceled, retained, abandoned bool
}

func newSubmissionRegistry() *submissionRegistry {
	return &submissionRegistry{owners: map[int64]*submissionOwner{}}
}
func (g *submissionRegistry) trackRun(run model.Run) *submissionOwner {
	r := &submissionOwner{withdrawn: make(chan struct{}), gate: g, sequence: run.QueueSequence, order: run.AdmissionSequence, repoID: run.RepoID, runID: run.ID, ref: run.Ref, sha: run.SHA, refKind: run.RefKind}
	if r.order <= 0 {
		r.order = r.sequence
	}
	g.mu.Lock()
	g.owners[r.sequence] = r
	g.mu.Unlock()
	return r
}
func (r *submissionOwner) beginSubmission(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g := r.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.abandoned {
		return nil, errSubmissionWithdrawn
	}
	if r.canceled || r.released {
		return nil, context.Canceled
	}
	r.prepared = true
	r.granted = true
	return r.release, nil
}
func (r *submissionOwner) cancel() {
	g := r.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.retained {
		return
	}
	r.canceled = true
	g.releaseLocked(r)
}
func (r *submissionOwner) release() {
	g := r.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	g.releaseLocked(r)
}
func (r *submissionOwner) releaseAfterCleanup() {
	g := r.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	r.retained = false
	g.releaseLocked(r)
}
func (r *submissionOwner) retain() {
	g := r.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.granted && !r.released {
		r.retained = true
	}
}
func (g *submissionRegistry) releaseLocked(r *submissionOwner) {
	if r.released || r.retained {
		return
	}
	r.released = true
	if g.owners[r.sequence] == r {
		delete(g.owners, r.sequence)
	}
}
func (g *submissionRegistry) abandon(sequence int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.owners[sequence]
	if r == nil {
		return false
	}
	if !r.canceled {
		r.canceled = true
		close(r.withdrawn)
	}
	if r.granted || r.retained {
		return false
	}
	r.abandoned = true
	r.canceled = true
	g.releaseLocked(r)
	return true
}

// Creation can wait on a source Pod indefinitely. A durable cancellation wakes
// that operation without releasing its cleanup custody before Create returns.
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
