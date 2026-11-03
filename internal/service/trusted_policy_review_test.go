package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
)

func TestTrustedPolicyPermanentFailureReleasesRun(t *testing.T) {
	for _, message := range []string{"permission denied", "repository archived", "blob exceeds size limit", "oberth git-ssh: host key, identity or configuration refused"} {
		for _, publication := range []bool{false, true} {
			name := message
			if publication {
				name += "/publication"
			}
			t.Run(name, func(t *testing.T) {
				f := newControlFixture(t, JobResult{Status: model.RunPassed})
				g := &policyTestGit{controlGit: f.git}
				f.scheduler.git = g
				if publication {
					f.scheduler.jobs = &policyChangingJobs{fakeJobs: f.jobs, afterBuild: func() { g.baselineErr = errors.New(message) }}
				} else {
					g.baselineErr = errors.New(message)
				}
				waits := 0
				f.scheduler.capacityWait = func(context.Context) error { waits++; return errors.New("unexpected permanent policy wait") }
				queued, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "policy-refusal", Repository: f.repo, Branch: "feature/refusal", SHA: strings.Repeat("a", 40), Actor: "agent"})
				if err != nil {
					t.Fatal(err)
				}
				if err := f.scheduler.ProcessNext(t.Context()); err != nil {
					t.Fatal(err)
				}
				run, err := f.store.Run(t.Context(), queued.ID)
				if err != nil {
					t.Fatal(err)
				}
				state := f.scheduler.AdmissionState()
				if run.Status != model.RunFailed || !strings.Contains(run.Error, message) || waits != 0 || state.Used != 0 || len(f.git.syncedBranches) != 0 {
					t.Fatalf("permanent failure hung or lost its cause/capacity: status=%s error=%q waits=%d used=%d", run.Status, run.Error, waits, state.Used)
				}
				pending, err := f.store.PendingPublications(t.Context())
				if err != nil || len(pending) != 0 {
					t.Fatalf("permanent refusal left pending publication: %v %v", pending, err)
				}
			})
		}
	}
}

func TestTrustedPolicyWaitHasTotalDeadline(t *testing.T) {
	f := newControlFixture(t)
	f.scheduler.policyWaitTimeout = 10 * time.Millisecond
	f.scheduler.git = &policyTestGit{controlGit: f.git, baselineErr: fmt.Errorf("%w: connection reset", gitcache.ErrPolicyUpstreamUnavailable)}
	f.scheduler.capacityWait = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	parent, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	queued, err := f.scheduler.EnqueueCI(parent, CIRequest{EventID: "policy-deadline", Repository: f.repo, Branch: "feature/deadline", SHA: strings.Repeat("a", 40), Actor: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.scheduler.ProcessNext(parent); err != nil {
		t.Fatal(err)
	}
	run, err := f.store.Run(t.Context(), queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != model.RunFailed || !strings.Contains(run.Error, "policy refresh deadline exceeded") || f.scheduler.AdmissionState().Used != 0 {
		t.Fatalf("policy wait did not expire and release capacity: %#v", run)
	}
}

func TestTrustedPolicyPublicationWaitHasDurableDeadline(t *testing.T) {
	f := newControlFixture(t)
	_, pub := beginPendingPromotionPublication(t, f, "main", strings.Repeat("b", 40), strings.Repeat("a", 40))
	f.scheduler.git = &policyTestGit{controlGit: f.git, baselineErr: fmt.Errorf("%w: connection reset", gitcache.ErrPolicyUpstreamUnavailable)}
	f.scheduler.policyWaitTimeout = time.Nanosecond
	result, err := f.scheduler.DeliverPublication(t.Context(), pub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Publication.Status != model.PublicationFailed || !strings.Contains(result.Publication.Error, "policy refresh deadline exceeded") || result.Publication.Attempts != 0 || len(f.git.promotions) != 0 {
		t.Fatalf("publication did not exhaust its policy deadline: %#v", result)
	}
}
