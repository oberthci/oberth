package service_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestLatestRunStoredCountsAndStepPage(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	steps := []stepmodel.RunStep{}
	for i := range 101 {
		steps = append(steps, stepmodel.RunStep{Path: fmt.Sprint(i), Status: stepmodel.StepPending})
	}
	f.store.native.SeedSteps(r.ID, steps)
	page, err := f.s.RunList(t.Context(), caller(), viewQuery{})
	if err != nil || len(page.Runs) != 1 || page.Runs[0].Total != 101 || len(page.Runs[0].Steps) != 0 {
		t.Fatalf("list %+v %v", page, err)
	}
	checkStepPages(t, f, r.ID)
}
func checkStepPages(t *testing.T, f *fixture, id string) {
	v, err := f.s.RunGet(t.Context(), caller(), viewQuery{ID: id, Limit: 100})
	if err != nil || v.Total != 101 || len(v.Steps) != 100 {
		t.Fatalf("page %+v %v", v, err)
	}
	if v.NextStepOffset == nil || *v.NextStepOffset != 100 {
		t.Fatal("missing step cursor")
	}
	v, err = f.s.RunGet(t.Context(), caller(), viewQuery{ID: id, Offset: 100})
	if err != nil || len(v.Steps) != 1 {
		t.Fatalf("continuation %+v %v", v, err)
	}
}
func TestRunListOneQueryNoPlanOrStepReads(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	before := f.store.Calls()
	_, err := f.s.RunList(t.Context(), caller(), viewQuery{})
	after := f.store.Calls()
	if err != nil || after["Runs"]-before["Runs"] != 1 || after["Steps"] != before["Steps"] || after["Run"] != before["Run"] {
		t.Fatalf("calls %v -> %v %v", before, after, err)
	}
}
func TestSelectorsIDPrefixCommitAndRef(t *testing.T) {
	t.Parallel()
	for _, q := range []viewQuery{{ID: runID}, {ID: runID[:8]}, {SHA: head}, {SHA: head[:8]}, {Ref: "refs/heads/topic"}, {SHA: head, Repo: "widget"}} {
		t.Run(fmt.Sprint(q), func(t *testing.T) {
			f := setup(t)
			f.seed(stepmodel.RunRunning)
			v, err := f.s.Status(t.Context(), caller(), q)
			if err != nil || v.ID != runID {
				t.Fatalf("selector %+v %v", v, err)
			}
		})
	}
}
func TestVisiblePrefixAmbiguity(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	r.ID = runID[:8] + "111111111111111111111111"
	f.store.native.SeedRun(r)
	_, err := f.s.Status(t.Context(), caller(), viewQuery{ID: runID[:8]})
	requireCode(t, err, publicerr.Ambiguous)
	r.Repo = "private/team/widget"
	f.store.native.SeedRun(r)
	v, err := f.s.Status(t.Context(), caller(), viewQuery{ID: runID[:8]})
	if err != nil || v.ID != runID {
		t.Fatalf("hidden ambiguity %+v %v", v, err)
	}
}
func TestCommitPrefixAmbiguityAndNewestSameCommit(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	r.ID = "abcdef02000000000000000000000002"
	r.Sequence = 2
	f.store.native.SeedRun(r)
	v, err := f.s.Status(t.Context(), caller(), viewQuery{SHA: head[:8]})
	if err != nil || v.ID != r.ID {
		t.Fatal("not newest", v, err)
	}
	sha := head[:8] + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	r.ObjectSHA = sha
	r.CommitSHA = &sha
	f.store.native.SeedRun(r)
	_, err = f.s.Status(t.Context(), caller(), viewQuery{SHA: head[:8]})
	requireCode(t, err, publicerr.Ambiguous)
}
func TestListFiltersBeforeCursorPagination(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	seedFilterRuns(f, r)
	q := viewQuery{RefGlob: "refs/heads/*", Ref: "refs/heads/topic", Own: true, Held: true, Status: stepmodel.RunRunning, Limit: 1}
	p, err := f.s.RunList(t.Context(), caller(), q)
	if err != nil || len(p.Runs) != 1 || p.Runs[0].ID != fmt.Sprintf("abcdef%026x", 6) {
		t.Fatalf("first %+v %v", p, err)
	}
	if p.NextCursor == "" {
		t.Fatal("missing cursor")
	}
	q.Before = p.NextCursor
	p, err = f.s.RunList(t.Context(), caller(), q)
	if err != nil || len(p.Runs) != 1 || p.Runs[0].ID != fmt.Sprintf("abcdef%026x", 1) {
		t.Fatalf("next %+v %v", p, err)
	}
}
func seedFilterRuns(f *fixture, r stateport.Run) {
	for i := range 6 {
		r.ID = fmt.Sprintf("abcdef%026x", i+1)
		r.Sequence = int64(i + 1)
		r.Ref = "refs/heads/topic"
		r.Origin.Actor = caller().Identity
		r.Origin.AssistRequested = true
		r.Origin.WorkflowUID = "workflow-uid"
		f.holds.setLive(r.ID, []model.LiveHold{fixtureHold(r.ID)})
		r.Status = stepmodel.RunRunning
		if i == 1 {
			r.Ref = "refs/heads/other"
		}
		if i == 2 {
			r.Status = stepmodel.RunFailed
		}
		if i == 3 {
			r.Origin.Actor = "other"
		}
		if i == 4 {
			f.holds.setLive(r.ID, nil)
		}
		f.store.native.SeedRun(r)
	}
}
func TestLatestIsPerRepositoryAndRef(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	for i := range 3 {
		r.ID = fmt.Sprintf("abcdef%026x", i+1)
		r.Sequence = int64(i + 2)
		if i == 2 {
			r.Ref = "refs/heads/other"
		}
		f.store.native.SeedRun(r)
	}
	p, err := f.s.RunList(t.Context(), caller(), viewQuery{Latest: true})
	if err != nil || len(p.Runs) != 2 {
		t.Fatalf("latest %+v %v", p, err)
	}
}

func TestBadRunSelectorsAndFilters(t *testing.T) {
	t.Parallel()
	fbad := setup(t)
	_, err := fbad.s.Service.PrepareWait(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.WaitArgs{SHA: "not-a-sha"})
	requireField(t, err, "sha")

	f := setup(t)
	f.seed(stepmodel.RunRunning)
	for _, q := range []viewQuery{{ID: "short"}, {ID: "ABCDEFGH"}, {ID: runID, Repo: "../bad"}} {
		_, err := f.s.Status(t.Context(), caller(), q)
		requireCode(t, err, publicerr.InvalidInput)
	}
	for _, q := range []viewQuery{{Status: "unknown"}, {RefGlob: "["}} {
		_, err := f.s.RunList(t.Context(), caller(), q)
		requireCode(t, err, publicerr.InvalidInput)
	}
}
func TestUnknownUpstreamNeverMeansAllRepositories(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	a := caller()
	a.Scope = []string{"unknown"}
	f.store.Actors[a.Fingerprint] = a
	p, err := f.s.RunList(t.Context(), a, viewQuery{})
	if err != nil || len(p.Runs) != 0 {
		t.Fatalf("scope %+v %v", p, err)
	}
}
func TestStepsKeepAuthoredContainersAndUnknownEvidence(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	f.store.native.SeedSteps(r.ID, []stepmodel.RunStep{{Path: "build", Status: stepmodel.StepUnknown, Detail: "native detail", Logs: []stepmodel.LogSelector{{Container: "main"}, {Container: "authored"}, {Container: "wait"}, {Container: "init"}, {Container: "oberth-executor-token"}, {Container: "init-artifact-source"}}}})
	v, err := f.s.RunGet(t.Context(), caller(), viewQuery{ID: r.ID})
	if err != nil || v.Steps[0].Status != stepmodel.StepUnknown || len(v.Steps[0].Logs) != 2 || v.Steps[0].Detail != "native detail" {
		t.Fatalf("steps %+v %v", v, err)
	}
	v, err = f.s.RunGet(t.Context(), caller(), viewQuery{ID: r.ID, Helpers: true})
	if err != nil || len(v.Steps[0].Logs) != 6 {
		t.Fatal("helpers", v, err)
	}
}
func TestReposDefaultExcludedAndLatestRun(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	f.git.Refs = []gitport.Ref{{Name: "refs/heads/main"}, {Name: "refs/heads/topic"}, {Name: "refs/tags/v1.0.0"}}
	rows, err := f.s.Repos(t.Context(), caller())
	if err != nil || len(rows) != 1 || rows[0].BranchCount == nil || *rows[0].BranchCount != 1 || rows[0].LatestRun == nil || rows[0].LatestRun.ID != runID {
		t.Fatalf("repos %+v %v", rows, err)
	}
}
func TestStatusUsesStoredCountsWithoutAdmissionProjection(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	r.LogicalStepCounts = stepmodel.LogicalStepCounts{Total: 42}
	r.Admission.Plan = admissioncontract.Plan{}
	f.store.native.SeedRun(r)
	v, err := f.s.Status(t.Context(), caller(), viewQuery{ID: r.ID})
	if err != nil || v.Total != 42 {
		t.Fatalf("counts recomputed %+v %v", v, err)
	}
}
func TestPreparedReadCancellation(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	call, err := f.s.PrepareRunList(t.Context(), caller(), viewQuery{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = call(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestEveryRunStateHasCompleteReason(t *testing.T) {
	t.Parallel()
	for _, status := range []stepmodel.RunStatus{stepmodel.RunQueued, stepmodel.RunRunning, stepmodel.RunPassed, stepmodel.RunFailed, stepmodel.RunCancelled} {
		f := setup(t)
		f.seed(status)
		v, err := f.s.Status(t.Context(), caller(), viewQuery{ID: runID})
		if err != nil || v.DisplayReason == "" {
			t.Fatalf("state %+v %v", v, err)
		}
		p, err := f.s.RunList(t.Context(), caller(), viewQuery{})
		if err != nil || len(p.Runs) != 1 || p.Runs[0].DisplayReason == "" {
			t.Fatalf("list %+v %v", p, err)
		}
	}
}

type branchReadHook struct {
	service.Git
	read func(context.Context, string) ([]gitport.Ref, error)
}

func (h branchReadHook) ListRefs(ctx context.Context, repo string) ([]gitport.Ref, error) {
	return h.read(ctx, repo)
}
func TestReposCountRepositoriesConcurrently(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := setup(t)
		f.store.native.SeedRepositories(nil)
		for i := range 6 {
			f.store.native.SeedRepository(stateport.Repository{Canonical: fmt.Sprintf("origin/team/repo%d", i)})
		}
		entered := make(chan string, 6)
		release := make(chan struct{})
		f.c.Git = branchReadHook{Git: f.git, read: func(_ context.Context, repo string) ([]gitport.Ref, error) {
			entered <- repo
			<-release
			return nil, nil
		}}
		f.restart(t)
		done := make(chan error, 1)
		go func() { _, err := f.s.Repos(t.Context(), caller()); done <- err }()
		synctest.Wait()
		n := len(entered)
		close(release)
		synctest.Wait()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if n != 6 {
			t.Fatalf("independent repository reads started %d, want 6", n)
		}
	})
}

func TestHeldListUsesLiveAvailabilityAfterNativeFailure(t *testing.T) {
	t.Parallel()
	f := heldFixture(t)
	r, err := f.store.Run(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	r.Status = stepmodel.RunFailed
	f.store.native.SeedRun(r)
	for _, status := range []stepmodel.RunStatus{"", stepmodel.RunFailed} {
		p, err := f.s.RunList(t.Context(), caller(), viewQuery{Held: true, Status: status, Limit: 1})
		if err != nil || len(p.Runs) != 1 || p.Runs[0].ID != r.ID {
			t.Fatal("live hold hidden by native result", p, err)
		}
	}
	f.holds.setLive(r.ID, nil)
	p, err := f.s.RunList(t.Context(), caller(), viewQuery{Held: true, Limit: 1})
	if err != nil || len(p.Runs) != 0 {
		t.Fatal("closed hold inferred from history", p, err)
	}
}
