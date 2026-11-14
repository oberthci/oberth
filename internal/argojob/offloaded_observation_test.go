package argojob

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func TestOffloadedNativeFinalReaders(t *testing.T) {
	for _, phase := range []wfv1.WorkflowPhase{wfv1.WorkflowSucceeded, wfv1.WorkflowFailed, wfv1.WorkflowError} {
		for _, path := range []string{"wait", "wait-resumed", "terminal", "resumed", "confirm", "settle"} {
			t.Run(string(phase)+"/"+path, func(t *testing.T) {
				plain := nativeCompletionFixture()
				wf := plain.DeepCopy()
				wf.Status.Nodes = nil
				wf.Status.OffloadNodeStatusVersion = "offloaded"
				wf.Status.Phase, wf.Status.Message = phase, "native verdict message"
				before := wf.DeepCopy()
				digest, hashErr := adoptionSpecHash(wf)
				if hashErr != nil {
					t.Fatal(hashErr)
				}
				binding := Adoption{UID: wf.UID, SpecSHA256: digest}
				controller := &Controller{workflows: &staticWorkflowClient{workflow: wf}, config: Config{Namespace: wf.Namespace}, hub: containerLogCache{workflow: wf}}
				var got Completion
				var err error
				switch path {
				case "wait-resumed":
					got, err = controller.WaitResumed(t.Context(), wf.Name, "run", io.Discard, binding)
				case "wait":
					got, err = controller.Wait(t.Context(), wf.Name, "run", io.Discard)
				case "terminal", "resumed":
					var result *Completion
					if path == "resumed" {
						result, err = controller.TerminalStateResumed(t.Context(), wf.Name, "run", binding, nil)
					} else {
						result, err = controller.TerminalState(t.Context(), wf.Name)
					}
					if result != nil {
						got = *result
					}
				case "confirm", "settle":
					var result *wfv1.Workflow
					var complete bool
					if path == "confirm" {
						result, complete, err = controller.confirmWatchedCompletion(t.Context(), plain, "run", binding)
					} else {
						result, complete = controller.settle(t.Context(), wf.Name, "run", plain)
					}
					if complete {
						t.Fatal("offloaded final read claimed complete nodes")
					}
					if result != nil {
						got = completionFor(result)
					}
				}
				if err != nil || !got.Terminal || got.Phase != string(phase) || got.Succeeded != (phase == wfv1.WorkflowSucceeded) {
					t.Fatalf("native verdict lost: %+v %v", got, err)
				}
				if got.ObservationComplete || len(got.NodeSteps) != 0 || !strings.Contains(got.Reason, "native verdict message") || !strings.Contains(got.Reason, "step rows are unavailable because Argo node status is offloaded") {
					t.Fatalf("missing evidence or reason: %+v", got)
				}
				if !reflect.DeepEqual(wf, before) {
					t.Fatal("shared Workflow mutated")
				}
			})
		}
	}
}

func TestOffloadedRunningWorkflowContinuesObservation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		running := nativeCompletionFixture()
		running.Status.Phase, running.Status.Nodes, running.Status.OffloadNodeStatusVersion = wfv1.WorkflowRunning, nil, "offloaded"
		final := running.DeepCopy()
		final.Status.Phase = wfv1.WorkflowSucceeded
		hub := newControlledWaitHub(running)
		controller := &Controller{workflows: &staticWorkflowClient{workflow: final}, config: Config{Namespace: running.Namespace}, hub: hub}
		done := make(chan Completion, 1)
		errs := make(chan error, 1)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { got, err := controller.Wait(ctx, running.Name, "run", io.Discard); done <- got; errs <- err }()
		synctest.Wait()
		select {
		case got := <-done:
			t.Fatalf("running observation stopped: %+v %v", got, <-errs)
		default:
		}
		hub.setWorkflow(final)
		synctest.Wait()
		got, err := <-done, <-errs
		if err != nil || !got.Succeeded || got.ObservationComplete {
			t.Fatal(got, err)
		}
	})
}

func TestOffloadedObservationKeepsIdentityChecks(t *testing.T) {
	wf := nativeCompletionFixture()
	wf.Status.Nodes, wf.Status.OffloadNodeStatusVersion = nil, "offloaded"
	controller := &Controller{workflows: &staticWorkflowClient{workflow: wf}, config: Config{Namespace: wf.Namespace}, hub: containerLogCache{workflow: wf}}
	if _, err := controller.Wait(t.Context(), wf.Name, "foreign-run", io.Discard); err == nil {
		t.Fatal("foreign run accepted")
	}
	if _, err := controller.TerminalStateResumed(t.Context(), wf.Name, "run", Adoption{UID: "foreign-uid"}, nil); err == nil {
		t.Fatal("foreign UID accepted")
	}
	observed := wf.DeepCopy()
	observed.UID = "foreign-uid"
	if _, _, err := controller.confirmWatchedCompletion(t.Context(), observed, "run", Adoption{}); err == nil {
		t.Fatal("replaced Workflow accepted")
	}
}
