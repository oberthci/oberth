package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/store"
)

func scheduledRunFixture(t *testing.T, repoName string) (*fixture, stateport.Run) {
	t.Helper()
	f := setup(t)
	state := landedStore(t, f)
	registerContractRepository(t, state, repoName)
	f.c.Runs, f.c.Catalog, f.c.Events = state, state, state
	f.restart(t)
	return f, acceptScheduledRun(t, f, repoName)
}

func acceptScheduledRun(t *testing.T, f *fixture, repoName string) stateport.Run {
	t.Helper()
	state := f.c.Runs.(*store.Store)
	repo, err := state.Repository(t.Context(), repoName, nil)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := nativebinding.CronName(repo.ID, "daily")
	if err != nil {
		t.Fatal(err)
	}
	id := parent + "-1795012440"
	request := model.CIRequest{ReceiveID: "child:" + id, Repo: repoName, Ref: "refs/heads/main", ObjectSHA: head, CommitSHA: new(head), Actor: admin(), Trigger: model.TriggerSchedule, ScheduleEntry: "daily"}
	prepared := landedAdmission(t, id, request)
	view := prepared.View()
	proof := nativebinding.ChildBinding{Run: nativebinding.RunBinding{RunID: id, RepositoryID: repo.ID, Ref: request.Ref, Trigger: "schedule", Revision: sourcecontract.Revision{Repository: repoName, Commit: head}, Identity: view.Digest, SpecSHA256: view.SpecDigest, Workflow: nativebinding.Object{Namespace: "ci", Name: id, UID: "child-uid"}}, Parent: nativebinding.Object{Namespace: "ci", Name: parent, UID: "parent-uid"}, ScheduleEntry: "daily", CreatedAt: at()}
	result, err := f.s.Accept(t.Context(), stateport.BatchAcceptance{ReceiveID: request.ReceiveID, Repository: repoName, Actor: request.Actor, Runs: []stateport.Acceptance{{Request: request, Prepared: prepared, Child: &proof}}})
	if err != nil || len(result.Runs) != 1 {
		t.Fatal("scheduled child acceptance", result, err)
	}
	retained, err := state.ChildProof(t.Context(), id)
	if err != nil || retained != proof {
		t.Fatal("scheduled child proof", retained, err)
	}
	return result.Runs[0]
}

type scheduledOperation struct {
	name string
	run  func(*testing.T, *fixture, apicontract.Caller, string) (string, error)
}

func scheduledOperations() []scheduledOperation {
	return []scheduledOperation{
		{"get", func(t *testing.T, f *fixture, c apicontract.Caller, id string) (string, error) {
			call, err := f.s.PrepareRunGet(t.Context(), c, apicontract.RunGetArgs{ID: id})
			out, err := execute(t.Context(), call, err)
			return out.ID, err
		}},
		{"wait", func(t *testing.T, f *fixture, c apicontract.Caller, id string) (string, error) {
			call, err := f.s.PrepareRunWait(t.Context(), c, apicontract.RunWaitArgs{ID: id})
			out, err := execute(t.Context(), call, err)
			if out.Run == nil {
				return "", err
			}
			return out.Run.ID, err
		}},
		{"wait_selector", func(t *testing.T, f *fixture, c apicontract.Caller, id string) (string, error) {
			call, err := f.s.Service.PrepareWait(t.Context(), c, apicontract.WaitArgs{ID: id})
			out, err := execute(t.Context(), call, err)
			if out.Run == nil {
				return "", err
			}
			return out.Run.ID, err
		}},
		{"logs", func(t *testing.T, f *fixture, c apicontract.Caller, id string) (string, error) {
			call, err := f.s.PrepareRunLogs(t.Context(), c, apicontract.RunLogsArgs{ID: id})
			out, err := execute(t.Context(), call, err)
			return out.ID, err
		}},
		{"cancel", func(t *testing.T, f *fixture, c apicontract.Caller, id string) (string, error) {
			call, err := f.s.PrepareRunCancel(t.Context(), c, apicontract.RunCancelArgs{ID: id})
			out, err := execute(t.Context(), call, err)
			return out.ID, err
		}},
		{"rerun", func(t *testing.T, f *fixture, c apicontract.Caller, id string) (string, error) {
			return scheduledRerun(t, f, c, id)
		}},
		{"purge", func(t *testing.T, f *fixture, c apicontract.Caller, id string) (string, error) {
			call, err := f.s.PrepareRunPurge(t.Context(), c, apicontract.RunPurgeArgs{ID: id, What: "logs", Reason: "retain test"})
			out, err := execute(t.Context(), call, err)
			return out.ID, err
		}},
		{"holds", func(t *testing.T, f *fixture, c apicontract.Caller, id string) (string, error) {
			call, err := f.s.PrepareRunHoldList(t.Context(), c, apicontract.RunHoldListArgs{ID: id})
			_, err = execute(t.Context(), call, err)
			return id, err
		}},
		{"status", func(t *testing.T, f *fixture, c apicontract.Caller, _ string) (string, error) {
			call, err := f.s.PrepareStatus(t.Context(), c, apicontract.StatusArgs{Ref: "refs/heads/main"})
			out, err := execute(t.Context(), call, err)
			return out.ID, err
		}},
		{"events", func(t *testing.T, f *fixture, c apicontract.Caller, id string) (string, error) {
			return scheduledEvents(t, f, c, id)
		}},
	}
}

func TestScheduledRunIDsUseRealScopedResolverForEveryOperation(t *testing.T) {
	t.Parallel()
	for _, op := range scheduledOperations() {
		for _, visible := range []bool{true, false} {
			name := "hidden"
			if visible {
				name = "visible"
			}
			t.Run(op.name+"/"+name, func(t *testing.T) { checkScheduledOperation(t, op, visible) })
		}
	}
}

func checkScheduledOperation(t *testing.T, op scheduledOperation, visible bool) {
	t.Helper()
	f, r := scheduledRunFixture(t, repository)
	finishScheduledOperation(t, f, op.name, r.ID)
	c := apicontract.Caller{Actor: admin()}
	c.Actor.Scope = []string{"origin"}
	if op.name == "purge" {
		c.Actor.Scope = nil
	}
	if !visible {
		c.Actor.Scope = []string{"private"}
	}
	f.store.Actors[c.Actor.Fingerprint] = c.Actor
	calls := 0
	real := f.c.Runs
	f.c.Runs = countedScheduledReads{RunStore: real, calls: &calls}
	f.restart(t)
	got, err := op.run(t, f, c, r.ID)
	if !visible {
		code := publicerr.NotFound
		if op.name == "purge" {
			code = publicerr.Forbidden
		}
		requireCode(t, err, code)
	} else if err != nil || got != r.ID {
		t.Fatal("scheduled operation refused exact child", got, err)
	}
	wantCalls := 1
	if !visible && op.name == "purge" {
		wantCalls = 0
	}
	if calls != wantCalls {
		t.Fatalf("scoped resolver calls=%d, want %d", calls, wantCalls)
	}
}

func TestScheduledRunIDsRejectMalformedAndNeverExpandNativePrefixes(t *testing.T) {
	t.Parallel()
	for _, op := range scheduledOperations() {
		if op.name == "status" {
			continue
		}
		t.Run(op.name, func(t *testing.T) {
			f, r := scheduledRunFixture(t, repository)
			finishScheduledOperation(t, f, op.name, r.ID)
			real := f.c.Runs
			calls := 0
			f.c.Runs = resolveHook{real, func(ctx context.Context, scope []string, id string) (stateport.Run, error) {
				calls++
				return real.ResolveRun(ctx, scope, id)
			}}
			f.restart(t)
			for _, id := range []string{strings.TrimSuffix(r.ID, "1795012440"), r.ID + "/other", r.ID + "-extra", strings.Replace(r.ID, "-1795012440", "-01795012440", 1)} {
				_, err := op.run(t, f, apicontract.Caller{Actor: admin()}, id)
				requireCode(t, err, publicerr.InvalidInput)
			}
			if calls != 0 {
				t.Fatal("malformed name reached resolver", calls)
			}
			// This is still a canonical timestamp, so only exact resolution can refuse it.
			_, err := op.run(t, f, apicontract.Caller{Actor: admin()}, r.ID[:len(r.ID)-1])
			requireCode(t, err, publicerr.NotFound)
			if calls != 1 {
				t.Fatal("canonical prefix bypassed scoped resolver", calls)
			}
		})
	}
}

func TestScheduledRunIDCannotResolveAnotherRepositoryChild(t *testing.T) {
	t.Parallel()
	f, r := scheduledRunFixture(t, "private/team/hidden")
	for _, op := range scheduledOperations() {
		t.Run(op.name, func(t *testing.T) {
			_, err := op.run(t, f, workerCaller(), r.ID)
			code := publicerr.NotFound
			if op.name == "purge" {
				code = publicerr.Forbidden
			}
			requireCode(t, err, code)
		})
	}
}

func TestScheduledRunIDsPreserveAgentActionPolicyAndHexSelectors(t *testing.T) {
	t.Parallel()
	f, r := scheduledRunFixture(t, repository)
	if err := f.s.Finished(t.Context(), service.FinishedRun{RunID: r.ID, Status: stepmodel.RunPassed}); err != nil {
		t.Fatal(err)
	}
	for _, op := range scheduledOperations() {
		t.Run(op.name, func(t *testing.T) {
			got, err := op.run(t, f, workerCaller(), r.ID)
			if code := scheduledAgentRefusal(op.name); code != "" {
				requireCode(t, err, code)
			} else if err != nil || got != r.ID {
				t.Fatal("scoped agent cannot read scheduled child", got, err)
			}
		})
	}
	call, err := f.s.Service.PrepareWait(t.Context(), workerCaller(), apicontract.WaitArgs{SHA: r.ID})
	if call != nil {
		t.Fatal("native run ID admitted as commit selector")
	}
	requireField(t, err, "sha")
}

func TestScheduledRunIDRoutesLogsToTheExactChild(t *testing.T) {
	t.Parallel()
	f, r := scheduledRunFixture(t, repository)
	logs := 0
	f.logs.ReadFunc = func(_ context.Context, got stateport.Run, q apicontract.RunLogsArgs) (apicontract.LogResult, error) {
		logs++
		if got.ID != r.ID || q.ID != r.ID || got.Origin.WorkflowUID != r.Origin.WorkflowUID {
			t.Fatal("wrong child log selection", got, q)
		}
		return apicontract.LogResult{Content: "child output"}, nil
	}
	call, err := f.s.PrepareRunLogs(t.Context(), workerCaller(), apicontract.RunLogsArgs{ID: r.ID})
	out, err := execute(t.Context(), call, err)
	if err != nil || out.ID != r.ID || out.Content != "child output" || logs != 1 {
		t.Fatal("scheduled log result", out, logs, err)
	}
}

func TestScheduledRunIDRoutesCancellationToTheExactChild(t *testing.T) {
	t.Parallel()
	f, r := scheduledRunFixture(t, repository)
	cancels := 0
	f.exec.CancelFunc = func(_ context.Context, got stateport.Run, terminate bool) error {
		cancels++
		if got.ID != r.ID || terminate || got.Origin.WorkflowUID != r.Origin.WorkflowUID {
			t.Fatal("wrong child cancellation", got, terminate)
		}
		return nil
	}
	call, err := f.s.PrepareRunCancel(t.Context(), apicontract.Caller{Actor: admin()}, apicontract.RunCancelArgs{ID: r.ID})
	out, err := execute(t.Context(), call, err)
	if err != nil || out.ID != r.ID || cancels != 1 {
		t.Fatal("scheduled cancellation", out, cancels, err)
	}
	state := f.c.Runs.(*store.Store)
	obligations, err := state.Obligations(t.Context(), r.ID)
	if err != nil || len(obligations) != 1 {
		t.Fatal("cancel obligation", obligations, err)
	}
	if obligations[0].RunID != r.ID || obligations[0].Value != stateport.ObligationStop {
		t.Fatal("wrong child obligation", obligations)
	}
}
