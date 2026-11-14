package service

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/storetest"
)

func TestReleaseCancelAndSecondBranchCancelUseNativeTerminate(t *testing.T) {
	for _, release := range []bool{false, true} {
		t.Run(map[bool]string{false: "second-branch-cancel", true: "release-first-cancel"}[release], func(t *testing.T) {
			f := newGroupFixture(t, 1)
			f.engine.holdStop = true
			spec := model.RunSpec{RepoID: f.exampleService.ID, RefKind: model.RefBranch, Ref: f.exampleService.DefaultBranch, SHA: strings.Repeat("a", 40), Actor: cancelAdmin.Identity, Trigger: "branch"}
			if release {
				spec.RefKind = model.RefTag
				spec.Ref = "v1.0.0"
				spec.Release = true
				spec.Trigger = "tag"
			}
			queued, err := storetest.EnqueueRun(f.store, t.Context(), spec)
			if err != nil {
				t.Fatal(err)
			}
			run, err := storetest.ClaimNextRun(f.store, t.Context())
			if err != nil {
				t.Fatal(err)
			}
			const workflow = "workflow-security"
			if _, err = f.store.SetRunWorkflowName(t.Context(), run.ID, workflow); err != nil {
				t.Fatal(err)
			}
			if err = f.engine.CreateWorkflow(t.Context(), WorkflowRequest{WorkflowName: workflow, Run: run}); err != nil {
				t.Fatal(err)
			}
			first := mustRunCancel(t, f.control, cancelAdmin, map[string]any{"id": queued.ID, "force": true})
			if first.Status != "running" {
				t.Fatalf("cancel terminalized before native completion: %+v", first)
			}
			f.engine.mu.Lock()
			terminations := len(f.engine.terminated)
			f.engine.mu.Unlock()
			if release && terminations != 1 {
				t.Fatalf("release cancel left a granted onExit runnable: native Terminate calls=%d", terminations)
			}
			if !release {
				if !strings.Contains(first.Warning, "repeat cancel") || !strings.Contains(first.Warning, "Terminate; exit handlers will be skipped") {
					t.Fatalf("first Stop acknowledgement omits escalation warning: %+v", first)
				}
				if terminations != 0 {
					t.Fatal("first ordinary branch cancellation must run exit handlers")
				}
				mustRunCancel(t, f.control, cancelAdmin, map[string]any{"id": queued.ID})
				f.engine.mu.Lock()
				terminations = len(f.engine.terminated)
				f.engine.mu.Unlock()
				if terminations != 1 {
					t.Fatalf("second cancel left onExit unbounded: native Terminate calls=%d", terminations)
				}
			}
			result, err := f.engine.Wait(t.Context(), workflow, nil)
			if err != nil {
				t.Fatal(err)
			}
			current, err := f.store.Run(t.Context(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.scheduler.finishObservedRun(t.Context(), current, f.exampleService, f.scheduler.requireMutation, result, runlog.Index{}, true); err != nil {
				t.Fatal(err)
			}
			final := f.run(t, run.ID)
			if final.Status != model.RunInterrupted || final.Phase != model.PhaseCancelled || model.DisplayRunStatus(final) != "cancelled" {
				t.Fatalf("native unsuccessful shutdown shown as CI failure: %+v", final)
			}
			f.assertNoCIIssue(t, f.exampleService, spec.Ref)
		})
	}
}
