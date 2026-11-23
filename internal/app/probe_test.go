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
	key := app.ProbeKey{Address: "forge:22", Fingerprint: "key", PinsDigest: "pins", Repository: request().Repository}
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
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&gitport.CloneUnfinishedError{Cause: context.DeadlineExceeded}, gitport.CloneUnfinishedReason},
		{&gitport.FetchUnfinishedError{Cause: context.DeadlineExceeded}, gitport.FetchUnfinishedReason},
	} {
		git := fake.Git{Remote: func(context.Context, string, string) (gitport.Ref, error) { return gitport.Ref{}, tc.err }}
		c := app.Checks{Git: git, Deadline: context.WithTimeout}
		result := c.CheckRepository(context.Background(), app.ProbeKey{Repository: request().Repository})
		if result.Reason != tc.want || result.Detail == "" {
			t.Fatal("an unfinished forge transfer was reported as another failure", result)
		}
	}
}

func TestProbeNamesLostForgeHistory(t *testing.T) {
	git := fake.Git{Remote: func(context.Context, string, string) (gitport.Ref, error) {
		return gitport.Ref{}, &gitport.ForgeHistoryLostError{}
	}}
	c := app.Checks{Git: git, Deadline: context.WithTimeout}
	result := c.CheckRepository(t.Context(), app.ProbeKey{Repository: request().Repository})
	if result.Reason != gitport.ForgeHistoryLostReason || result.Detail != (&gitport.ForgeHistoryLostError{}).Error() {
		t.Fatal("lost history did not retain its restoration remedy", result)
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
