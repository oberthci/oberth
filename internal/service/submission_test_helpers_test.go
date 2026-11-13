package service

import (
	"context"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

type submissionTestView struct {
	Runs         int
	Reservations []submissionTestOwner
}
type submissionTestOwner struct{ RunID, State string }

func (s *Scheduler) submissionStateForTest() submissionTestView {
	g := s.submissions
	g.mu.Lock()
	defer g.mu.Unlock()
	v := submissionTestView{Runs: len(g.owners)}
	seen := map[string]bool{}
	for _, r := range g.owners {
		seen[r.runID] = true
		v.Reservations = append(v.Reservations, submissionTestOwner{RunID: r.runID})
	}
	s.createMu.Lock()
	defer s.createMu.Unlock()
	for id := range s.createPending {
		if !seen[id] {
			v.Reservations = append(v.Reservations, submissionTestOwner{RunID: id})
			v.Runs++
		}
	}
	return v
}
func (s *Scheduler) ownerForTest(sequence int64) *submissionTestOwner {
	g := s.submissions
	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.owners[sequence]
	if r == nil {
		return nil
	}
	return &submissionTestOwner{RunID: r.runID}
}

type pausedCheckout struct {
	WorkspaceGit
	resume <-chan struct{}
}

func (g pausedCheckout) Checkout(ctx context.Context, repo, sha, directory string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-g.resume:
	}
	return g.WorkspaceGit.Checkout(ctx, repo, sha, directory)
}
func (f *groupFixture) pausePreparation(t *testing.T) func() {
	t.Helper()
	resume := make(chan struct{})
	f.scheduler.git = pausedCheckout{WorkspaceGit: f.scheduler.git, resume: resume}
	return func() { close(resume) }
}
func (f *groupFixture) awaitPreparing(t *testing.T, run model.Run) {
	t.Helper()
	until := time.Now().Add(testStartTimeout(15 * time.Second))
	for {
		r, err := f.store.Run(t.Context(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if r.Status == model.RunRunning && r.StartedAt == nil && r.WorkflowName == "" {
			return
		}
		if time.Now().After(until) {
			t.Fatalf("run never prepared: %+v", r)
		}
		time.Sleep(testTimeout(time.Millisecond))
	}
}
