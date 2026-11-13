package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

type selectiveAdoptionJobs struct {
	*adoptingJobs
	refused string
}

func (j *selectiveAdoptionJobs) Adopt(ctx context.Context, run model.Run) (bool, error) {
	if run.ID == j.refused {
		return false, errors.Join(ErrAdoptionRefused, errors.New("workflow capability changed or was revoked"))
	}
	return j.adoptingJobs.Adopt(ctx, run)
}

func TestStartupAdoptsOtherWorkflowsAfterOneBindingRefusal(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid-grants", true: "one-changed-grant"}[refuse], func(t *testing.T) {
			f := newRecoveryFixture(t)
			first := f.enqueueAndClaim(t, "feature/first", strings.Repeat("a", 40))
			second := f.enqueueAndClaim(t, "feature/second", strings.Repeat("b", 40))
			jobs := &selectiveAdoptionJobs{adoptingJobs: &adoptingJobs{found: true, recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal, waitBlocks: make(chan struct{}), waitEntered: make(chan struct{})}}}
			if refuse {
				jobs.refused = first.ID
			}
			s := f.scheduler(t, jobs)
			for i, run := range []model.Run{first, second} {
				f.setWorkflowName(t, run.ID, []string{"workflow-first", "workflow-second"}[i])
				if _, err := f.store.MarkRunSubmitted(t.Context(), run.ID); err != nil {
					t.Fatal(err)
				}
				file, err := s.logs.Create(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err = file.Close(); err != nil {
					t.Fatal(err)
				}
			}
			recovered := make(chan struct{})
			s.onStartupRecovered = func() { close(recovered) }
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- s.Run(ctx) }()
			defer func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(testStartTimeout(5 * time.Second)):
					t.Error("scheduler did not join")
				}
			}()
			select {
			case <-recovered:
			case err := <-done:
				t.Fatal("refused Workflow stopped scheduler startup", err)
			case <-time.After(testStartTimeout(5 * time.Second)):
				t.Fatal("scheduler did not finish recovery")
			}
			select {
			case <-jobs.waitEntered:
			case <-time.After(testStartTimeout(5 * time.Second)):
				t.Fatal("other Workflow was not observed")
			}
			for _, run := range []model.Run{first, second} {
				current, err := f.store.Run(t.Context(), run.ID)
				want := model.RunRunning
				if run.ID == jobs.refused {
					want = model.RunFailed
					if !strings.Contains(current.Error, "recovery refused for this run") || !strings.Contains(current.Error, "workflow-first") {
						t.Fatal("refusal lost its reason or Workflow", current)
					}
				}
				if err != nil || current.Status != want {
					t.Fatal("one refusal affected another run", current, err)
				}
			}
			jobs.mu.Lock()
			deleted := len(jobs.deleted)
			jobs.mu.Unlock()
			if deleted != 0 {
				t.Fatal("adoption refusal authorized Workflow deletion")
			}
		})
	}
}
