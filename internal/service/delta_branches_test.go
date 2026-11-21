package service_test

import (
	"context"
	"fmt"
	"github.com/oberthci/oberth/internal/apicontract"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	statefake "github.com/oberthci/oberth/internal/stateport/fake"
	"github.com/oberthci/oberth/internal/stepmodel"
	"strings"
	"testing"
	"testing/synctest"
)

func TestInstallationAdminRefusalExplainsRequiredAuthority(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed("failed")
	_, err := f.s.PrepareRunPurge(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.RunPurgeArgs{ID: runID, What: "logs", Reason: "cleanup"})
	p := publicerr.Classify(err)
	if p.Code() != publicerr.Forbidden || !strings.Contains(p.Reason(), "installation administrator") {
		t.Fatal("admin refusal lost its explanation", err)
	}
}
func TestStoppedServiceDoesNotReadAuthority(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed("running")
	if err := f.s.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	before := f.store.Calls()["Current"]
	_, err := f.s.PrepareRunGet(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.RunGetArgs{ID: runID})
	requireCode(t, err, publicerr.Unavailable)
	if f.store.Calls()["Current"] != before {
		t.Fatal("stopped service read authority")
	}
}
func TestWaitIDCannotChangeTrigger(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed("running")
	_, err := f.s.Service.PrepareWait(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.WaitArgs{ID: runID, Trigger: "tag"})
	requireCode(t, err, publicerr.NotFound)
}
func TestRunListDefaultFiftyAndInvalidBefore(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed("passed")
	for i := range 51 {
		r.ID = fmt.Sprintf("%032x", i+256)
		r.Sequence = 0
		f.store.native.SeedRun(r)
	}
	call, err := f.s.Service.PrepareRunList(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.RunListArgs{})
	if err != nil {
		t.Fatal(err)
	}
	p, err := call(t.Context())
	if err != nil || len(p.Runs) != 50 || p.NextCursor == "" {
		t.Fatal("default page is not fifty", p, err)
	}
	_, err = f.s.Service.PrepareRunList(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.RunListArgs{Before: "bad"})
	requireField(t, err, "before")
}
func TestRepoListDefaultHidesArchived(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.archiveRepository(t, repository)
	c := apicontract.Caller{Actor: caller()}
	call, err := f.s.PrepareRepoList(t.Context(), c, apicontract.RepoListArgs{})
	if err != nil {
		t.Fatal(err)
	}
	p, err := call(t.Context())
	if err != nil || len(p.Repositories) != 0 {
		t.Fatal("archived repository leaked into default list", p, err)
	}
	call, err = f.s.PrepareRepoList(t.Context(), c, apicontract.RepoListArgs{Archived: "include"})
	if err != nil {
		t.Fatal(err)
	}
	p, err = call(t.Context())
	if err != nil || len(p.Repositories) != 1 {
		t.Fatal("explicit archive inclusion failed", p, err)
	}
}

type stateReadHook struct {
	service.RunStore
	read func(context.Context, string) (stateport.RunState, error)
}

func (h stateReadHook) RunState(ctx context.Context, id string) (stateport.RunState, error) {
	return h.read(ctx, id)
}
func TestWaitRearmsAfterNonterminalWake(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := setup(t)
		f.seed("running")
		reads := 0
		f.c.Runs = stateReadHook{RunStore: f.store, read: func(ctx context.Context, id string) (stateport.RunState, error) {
			reads++
			if reads > 3 {
				return stateport.RunState{}, fmt.Errorf("closed wake channel was reused")
			}
			return f.store.RunState(ctx, id)
		}}
		f.restart(t)
		call, err := f.s.PrepareRunWait(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.RunWaitArgs{ID: runID, TimeoutS: 5})
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan waitOutcome, 1)
		go func() { r, e := call(t.Context()); done <- waitOutcome{r, e} }()
		synctest.Wait()
		if err := f.store.native.Progress(t.Context(), statefake.NativeProgress{ID: runID, Revision: 1, Status: stepmodel.RunRunning, Detail: "progress"}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if reads != 1 {
			t.Fatal("waiter reread a closed wake channel", reads)
		}
		if err := f.s.Finished(t.Context(), service.FinishedRun{RunID: runID, Status: stepmodel.RunPassed}); err != nil {
			t.Fatal(err)
		}
		result := <-done
		if result.err != nil || result.result.Run == nil || result.result.Run.Status != stepmodel.RunPassed || result.result.TimedOut || reads != 2 {
			t.Fatal("second committed wake was lost", result, reads)
		}
	})
}

type progressFinish struct {
	service.RunStore
	native *statefake.MovingRuns
}

func (h progressFinish) Finish(ctx context.Context, a stateport.Action, u stateport.RunUpdate) error {
	if err := h.native.Progress(ctx, statefake.NativeProgress{ID: u.ID, Revision: u.Revision, Status: stepmodel.RunRunning, Detail: "concurrent progress"}); err != nil {
		return err
	}
	return h.RunStore.Finish(ctx, a, u)
}
func TestNativeProgressBetweenFinishReadAndCASDoesNotNeedRetry(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed("running")
	f.c.Runs = progressFinish{RunStore: f.store, native: f.store.native}
	f.restart(t)
	if err := f.s.Finished(t.Context(), service.FinishedRun{RunID: runID, Status: stepmodel.RunPassed}); err != nil {
		t.Fatal(err)
	}
	r, err := f.store.Run(t.Context(), runID)
	if err != nil || r.Status != stepmodel.RunPassed || f.store.Calls()["Finish"] != 1 {
		t.Fatal("native progress defeated single Finish", r, err)
	}
}
