package service

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type unavailablePrioritySizeStore struct {
	*store.Store
	repoID      int64
	lookupErr   error
	unavailable atomic.Bool
	calls       atomic.Int64
}

func (s *unavailablePrioritySizeStore) MaxRunSize(ctx context.Context, repoID int64) (periapsis.Size, error) {
	s.calls.Add(1)
	if repoID == s.repoID && s.unavailable.Load() {
		return "", s.lookupErr
	}
	return s.Store.MaxRunSize(ctx, repoID)
}

type priorityObservingReleaseJobs struct {
	*fakeJobs
	scheduler *Scheduler
	priority  model.RunPriority
	reason    string
}

func (jobs *priorityObservingReleaseJobs) CreateRelease(ctx context.Context, request JobRequest) error {
	var found bool
	jobs.priority, jobs.reason, found = jobs.scheduler.AdmissionPriority(request.Run.QueueSequence)
	if !found {
		return fmt.Errorf("release has no admission priority")
	}
	return jobs.fakeJobs.CreateRelease(ctx, request)
}

func TestPrioritySizeLookupFailureDoesNotFailReleaseExecution(t *testing.T) {
	for _, size := range []periapsis.Size{periapsis.S, periapsis.M, periapsis.XL} {
		for _, lookupErr := range []error{prioritySQLiteError(5), context.DeadlineExceeded} {
			t.Run(fmt.Sprintf("%s/%v", size, lookupErr), func(t *testing.T) {
				f := newControlFixture(t, JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)}})
				f.jobs.size = size
				state := &unavailablePrioritySizeStore{Store: f.store, repoID: f.repo.ID, lookupErr: lookupErr}
				state.unavailable.Store(true)
				f.scheduler.store = state
				jobs := &priorityObservingReleaseJobs{fakeJobs: f.jobs, scheduler: f.scheduler}
				f.scheduler.releaseJobs = jobs
				tag, err := f.scheduler.AdmitRelease(t.Context(), ReleaseRequest{EventID: "size-read-failure", Repository: f.repo, Tag: "v1", ObjectSHA: strings.Repeat("b", 40), CommitSHA: strings.Repeat("c", 40), Actor: "pusher"})
				if err != nil {
					t.Fatal(err)
				}
				if err := f.scheduler.ProcessNext(t.Context()); err != nil {
					t.Fatal(err)
				}
				finished, err := f.store.Run(t.Context(), tag.ID)
				if err != nil || finished.Status != model.RunPassed || finished.Error != "" || finished.StartedAt == nil {
					t.Fatalf("priority lookup failed the release: %+v %v", finished, err)
				}
				if state.calls.Load() == 0 || len(jobs.createdReleases) != 1 || len(f.git.syncedTags) != 1 {
					t.Fatalf("lookup/creation/publication = %d/%d/%d", state.calls.Load(), len(jobs.createdReleases), len(f.git.syncedTags))
				}
				if jobs.priority != model.PriorityNormal || !strings.Contains(jobs.reason, "repository size limit unavailable") {
					t.Fatalf("unavailable lookup retained urgency: %s / %s", jobs.priority, jobs.reason)
				}
			})
		}
	}
}

func TestPrioritySizeLookupRefreshWithholdsBoostAndRecovers(t *testing.T) {
	f := newControlFixture(t)
	ctx := t.Context()
	if _, err := f.store.SetSizeGrant(ctx, f.repo.ID, "admin", periapsis.XL); err != nil {
		t.Fatal(err)
	}
	other, err := f.store.CreateRepository(ctx, model.RepositorySpec{Name: "other", UpstreamID: f.repo.UpstreamID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	g := newWeightedAdmission(4)
	f.scheduler.admission = g
	holder := g.reserve(-1)
	release, err := holder.acquire(ctx, periapsis.XL)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	reserve := func(repo model.Repository) *admissionReservation {
		t.Helper()
		tag, err := f.scheduler.AdmitRelease(ctx, ReleaseRequest{EventID: repo.Name, Repository: repo, Tag: "v1", ObjectSHA: strings.Repeat("b", 40), CommitSHA: strings.Repeat("c", 40), Actor: "pusher"})
		if err != nil {
			t.Fatal(err)
		}
		r := g.reserveRun(tag.Run)
		t.Cleanup(r.cancel)
		g.mu.Lock()
		r.prepared, r.size, r.weight = true, periapsis.S, 1
		g.mu.Unlock()
		return r
	}
	r, unaffected := reserve(f.repo), reserve(other)
	state := &unavailablePrioritySizeStore{Store: f.store, repoID: f.repo.ID, lookupErr: context.DeadlineExceeded}
	f.scheduler.store = state
	check := func(want model.RunPriority, reason string) {
		t.Helper()
		if err := f.scheduler.refreshPriorities(ctx); err != nil {
			t.Fatalf("advisory size lookup aborted refresh: %v", err)
		}
		p, why, found := f.scheduler.AdmissionPriority(r.sequence)
		if !found || p != want || !strings.Contains(why, reason) {
			t.Fatalf("refreshed priority=%s / %s, want %s / %s", p, why, want, reason)
		}
		if p, _, _ := f.scheduler.AdmissionPriority(unaffected.sequence); p != model.PriorityHighest {
			t.Fatalf("another repository lost its boost: %s", p)
		}
	}
	check(model.PriorityHighest, "oldest waiting")
	state.unavailable.Store(true)
	check(model.PriorityNormal, "repository size limit unavailable")
	// Even an S tag must not inherit default-L urgency when the read fails.
	control, err := NewAPI(APIConfig{Runs: f.store, Repositories: f.store, Admission: f.scheduler})
	if err != nil {
		t.Fatal(err)
	}
	durable, err := f.store.Run(ctx, r.runID)
	if err != nil {
		t.Fatal(err)
	}
	shown := control.presentationRun(ctx, durable)
	if shown.Priority != model.PriorityNormal || !strings.Contains(shown.PriorityReason, "size limit unavailable") || durable.Status != model.RunQueued {
		t.Fatalf("priority failure changed run state or lost reason: %+v / %+v", shown, durable)
	}
	g.mu.Lock()
	r.priorityOverride, r.priorityOverrideReason = model.PriorityHigh, "intentional expedite"
	explicit := r.priorityRun()
	r.priorityOverride, r.priorityOverrideReason = "", ""
	aged := r.priorityRun()
	aged.QueuedAt = time.Now().Add(-time.Hour)
	g.mu.Unlock()
	if explicit.Priority != model.PriorityHigh || explicit.PriorityReason != "admin bump: intentional expedite" {
		t.Fatalf("lookup failure discarded explicit bump: %+v", explicit)
	}
	if p, _ := aged.EffectivePriority(time.Now()); p != model.PriorityHighest {
		t.Fatal("lookup failure disabled starvation aging")
	}
	state.unavailable.Store(false)
	check(model.PriorityHighest, "oldest waiting")
}
