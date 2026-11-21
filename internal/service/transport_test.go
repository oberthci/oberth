package service_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// Every binding uses the real service method and the producer's exact I/O identity.
func TestRealTransportBindingsCompile(t *testing.T) {
	t.Parallel()
	f := setup(t)
	bindings := f.s.Bindings()

	n := 0
	for _, d := range apicontract.Definitions(bindings) {
		if !d.Bound {
			t.Fatal("unbound service tool", d.Name)
		}
		n++
	}
	if n != 74 {
		t.Fatalf("bindings %d", n)
	}
}
func TestTransportPreparationRejectsHiddenLogBeforeStreaming(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed("running")
	r.Repo = "private/team/widget"
	f.store.native.SeedRun(r)
	call, err := f.s.PrepareRunLogs(t.Context(), apicontract.Caller{Actor: caller(), CredentialID: "private"}, apicontract.RunLogsArgs{ID: runID})
	if err == nil || call != nil {
		t.Fatal("hidden log passed preparation")
	}
}
func TestTransportLogCancellationJoinsReader(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := setup(t)
		f.seed("running")
		started := make(chan struct{})
		stopped := make(chan struct{})
		f.logs.ReadFunc = func(ctx context.Context, _ stateport.Run, _ apicontract.RunLogsArgs) (apicontract.LogResult, error) {
			close(started)
			defer close(stopped)
			<-ctx.Done()
			return apicontract.LogResult{}, ctx.Err()
		}
		call, err := f.s.PrepareRunLogs(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.RunLogsArgs{ID: runID})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { _, err := call(ctx); done <- err }()
		<-started
		cancel()
		synctest.Wait()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		default:
			t.Fatal("call did not join reader")
		}
		select {
		case <-stopped:
		default:
			t.Fatal("reader still running")
		}
	})
}
func TestTransportAuthorityLookupIsTheOnlyClassification(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		want apicontract.Access
	}{{"publication_list", apicontract.Read}, {"receive_outbox", apicontract.ScopedAdmin}, {"run_purge", apicontract.Admin}, {"repo_list", apicontract.Read}} {
		got, ok := apicontract.Required(tc.name)
		if !ok || got != tc.want {
			t.Fatalf("%s class %s", tc.name, got)
		}
	}
}
func TestUnscopedNonAdministratorCannotPurge(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed("failed")
	a := caller()
	a.Scope = nil
	f.store.Actors[a.Fingerprint] = a
	call, err := f.s.PrepareRunPurge(t.Context(), apicontract.Caller{Actor: a}, apicontract.RunPurgeArgs{ID: runID, What: "logs", Reason: "space"})
	if err == nil || call != nil {
		t.Fatal("unscoped non-administrator prepared purge")
	}
}
func TestRunPrefixAtEightAndBelow(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed("running")
	for _, n := range []int{8, 7} {
		call, err := f.s.PrepareRunGet(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.RunGetArgs{ID: runID[:n]})
		if n == 7 {
			if err == nil || call != nil {
				t.Fatal("seven-character ID accepted")
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
}
func TestWaitTriggerChoosesMatchingRun(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := setup(t)
		branch := f.seed("passed")
		tag := branch.Clone()
		tag.ID = "bbbbbbbb000000000000000000000002"
		tag.Sequence = 2
		tag.Origin.Trigger = "tag"
		tag.Ref = "refs/tags/check"
		tag.Status = "running"
		f.store.native.SeedRun(tag)
		call, err := f.s.Service.PrepareWait(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.WaitArgs{SHA: head, Trigger: "branch"})
		if err != nil {
			t.Fatal(err)
		}
		out, err := call(t.Context())
		if err != nil || out.Run == nil || out.Run.ID != branch.ID {
			t.Fatalf("trigger %+v %v", out, err)
		}
		_, err = f.s.Service.PrepareWait(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.WaitArgs{SHA: head, Trigger: "unknown"})
		requireField(t, err, "trigger")
	})
}
func TestTypedViewAndTimeoutRefusalsNameFields(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed("running")
	c := apicontract.Caller{Actor: caller()}
	_, err := f.s.PrepareRunGet(t.Context(), c, apicontract.RunGetArgs{ID: runID, View: "unknown"})
	requireField(t, err, "view")
	_, err = f.s.Service.PrepareRunList(t.Context(), c, apicontract.RunListArgs{View: "unknown"})
	requireField(t, err, "view")
	_, err = f.s.PrepareRepoList(t.Context(), c, apicontract.RepoListArgs{Archived: "unknown"})
	requireField(t, err, "archived")
}
func TestReleaseRerunWarnsWithoutForce(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed("failed")
	r.Release = true
	r.Origin.Trigger = "tag"
	r.Ref = "refs/tags/v1.0.0"
	f.store.native.SeedRun(r)
	call, err := f.s.PrepareRunRerun(t.Context(), apicontract.Caller{Actor: admin()}, apicontract.RunRerunArgs{ID: runID})
	if err != nil {
		t.Fatal(err)
	}
	out, err := call(t.Context())
	if err != nil || out.Warning != "release steps may run again" {
		t.Fatalf("rerun %+v %v", out, err)
	}
}

func TestBindingsDispatchCanonicalRunGet(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunPassed)
	for _, d := range apicontract.Definitions(f.s.Bindings()) {
		if d.Name != "run_get" {
			continue
		}
		input := d.NewInput().(*apicontract.RunGetArgs)
		input.ID = runID
		call, err := d.Prepare(t.Context(), apicontract.Caller{Actor: caller()}, input)
		if err != nil {
			t.Fatal(err)
		}
		output, err := call(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		run, ok := output.(apicontract.RunRecord)
		if !ok || run.ID != runID || run.Repo != repository || run.Status != stepmodel.RunPassed {
			t.Fatal("binding did not dispatch exact service handler", output)
		}
		return
	}
	t.Fatal("canonical run_get is absent")
}
