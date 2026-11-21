package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/app/fake"
	"github.com/oberthci/oberth/internal/gitport"
)

// Covers: REQ-C3-003
func TestHonestRepositoryProbesContract(t *testing.T) {
	t.Parallel()
	fail := true
	git := fake.Git{Remote: func(_ context.Context, repo, ref string) (gitport.Ref, error) {
		if ref != "HEAD" || repo != request().Repository {
			t.Fatal("probe did not use Git HEAD")
		}
		if fail {
			return gitport.Ref{}, errors.New("ERR repository denied")
		}
		return request().Ref, nil
	}}
	c := app.Checks{Git: git, Deadline: context.WithTimeout}
	key := app.ProbeKey{Address: "forge:22", User: "git", Fingerprint: "key", PinsDigest: "pins", Repository: request().Repository}
	result := c.CheckRepository(context.Background(), key)
	if result.Reason != "repository check failed" || result.Detail != "ERR repository denied" {
		t.Fatal(result)
	}
	fail = false
	current := c.CheckRepository(context.Background(), key)
	if current.Reason != "repository is readable" || current.Detail != "" {
		t.Fatal(current)
	}
}

func TestProbeOfUnfinishedFirstCloneSaysSo(t *testing.T) {
	t.Parallel()
	git := fake.Git{Remote: func(context.Context, string, string) (gitport.Ref, error) {
		return gitport.Ref{}, &gitport.CloneUnfinishedError{Cause: context.DeadlineExceeded}
	}}
	c := app.Checks{Git: git, Deadline: context.WithTimeout}
	result := c.CheckRepository(context.Background(), app.ProbeKey{Repository: request().Repository})
	if result.Reason != gitport.CloneUnfinishedReason || result.Detail == "" {
		t.Fatal("an unfinished first clone was reported as another failure", result)
	}
}

func TestProbeDeadlineActuallyCancelsTransportAtFifteenSeconds(t *testing.T) {
	t.Parallel()
	clock := newDeadlineClock()
	entered := make(chan struct{})
	c := app.Checks{Deadline: clock.deadline, Git: fake.Git{Remote: func(ctx context.Context, _, _ string) (gitport.Ref, error) {
		close(entered)
		<-ctx.Done()
		return gitport.Ref{}, context.Cause(ctx)
	}}}
	done := make(chan app.ProbeResult, 1)
	go func() {
		done <- c.CheckRepository(context.Background(), app.ProbeKey{Repository: request().Repository})
	}()
	call := <-clock.calls
	<-entered
	beforeDeadline(t, clock, call, 15*time.Second)
	result := <-done
	if result.Reason != "repository check failed" || result.Detail != context.DeadlineExceeded.Error() {
		t.Fatal(result)
	}
}
