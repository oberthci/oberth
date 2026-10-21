package argojob

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func adoptionWorkflow() (*wfv1.Workflow, AdoptionRequest) {
	wf := budgetTestWorkflow()
	wf.UID = "workflow-uid"
	wf.Annotations[identityAnnotation] = strings.Repeat("a", 64)
	wf.Annotations[refAnnotation] = "feature/adopt"
	wf.Annotations[argoworkflow.SizeAnnotation] = "L"
	wf.Annotations[argoworkflow.ConcurrencyGroupAnnotation] = "build"
	wf.Labels = map[string]string{"oberth.ci/sha": strings.Repeat("b", 40), "oberth.ci/trigger": string(periapsis.TriggerCI)}
	return wf, AdoptionRequest{Name: wf.Name, RunID: wf.Annotations[runIDAnnotation], Ref: "feature/adopt", SHA: strings.Repeat("b", 40), Group: "build", Trigger: periapsis.TriggerCI, RunTrigger: argoworkflow.RunTriggerBranch}
}

func TestAdoptionBindsDurableRunAndSubmittedResources(t *testing.T) {
	wf, request := adoptionWorkflow()
	for _, test := range []struct {
		name   string
		mutate func(*wfv1.Workflow)
	}{
		{"valid", func(*wfv1.Workflow) {}},
		{"foreign-run", func(wf *wfv1.Workflow) { wf.Annotations[runIDAnnotation] = "other" }},
		{"missing-spec-identity", func(wf *wfv1.Workflow) { delete(wf.Annotations, identityAnnotation) }},
		{"missing-uid", func(wf *wfv1.Workflow) { wf.UID = "" }},
		{"foreign-ref", func(wf *wfv1.Workflow) { wf.Annotations[refAnnotation] = "main" }},
		{"foreign-sha", func(wf *wfv1.Workflow) { wf.Labels["oberth.ci/sha"] = "other" }},
		{"foreign-trigger", func(wf *wfv1.Workflow) { wf.Labels["oberth.ci/trigger"] = "release" }},
		{"foreign-group", func(wf *wfv1.Workflow) { wf.Annotations[argoworkflow.ConcurrencyGroupAnnotation] = "other" }},
		{"bad-size", func(wf *wfv1.Workflow) { wf.Annotations[argoworkflow.SizeAnnotation] = "unbounded" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := wf.DeepCopy()
			test.mutate(candidate)
			controller := &Controller{workflows: &staticWorkflowClient{workflow: candidate}, config: Config{Namespace: wf.Namespace}}
			binding, found, err := controller.Adopt(context.Background(), request)
			if test.name == "valid" {
				if err != nil || !found || binding.Size != periapsis.L || binding.UID != wf.UID || binding.Identity != wf.Annotations[identityAnnotation] {
					t.Fatalf("adoption = %#v, %v, %v", binding, found, err)
				}
			} else if err == nil {
				t.Fatalf("invalid adoption accepted: %#v, %v", binding, found)
			}
		})
	}
	controller := &Controller{workflows: missingWorkflowClient{}, config: Config{Namespace: wf.Namespace}}
	if _, found, err := controller.Adopt(context.Background(), request); err != nil || found {
		t.Fatalf("missing Workflow = %v, %v", found, err)
	}
}

func TestAdoptedWaitReplaysOnlyUnrecordedStepsAndRestoresBudget(t *testing.T) {
	wf, request := adoptionWorkflow()
	calls := map[string]int{}
	controller := &Controller{workflows: &staticWorkflowClient{workflow: wf}, config: Config{Namespace: wf.Namespace, MaxRunLogBytes: 4096}, interval: time.Millisecond,
		logs: func(_ context.Context, _, pod, _ string) (io.ReadCloser, error) {
			calls[pod]++
			return io.NopCloser(strings.NewReader("new log line\n")), nil
		}}
	binding, found, err := controller.Adopt(context.Background(), request)
	if err != nil || !found {
		t.Fatalf("adopt: %v %v", found, err)
	}
	steps := StepResults(wf)
	event := func(step StepResult) runprogress.Event {
		return runprogress.Event{Version: runprogress.Version, Burn: step.Burn, Step: step.Step, Ordinal: step.Ordinal, Status: step.Status, StartedAt: step.StartedAt, FinishedAt: step.FinishedAt}
	}
	var output bytes.Buffer
	completion, err := controller.WaitResumed(context.Background(), wf.Name, request.RunID, &output, []runprogress.Event{event(steps[0])}, 100, binding)
	if err != nil || !completion.Succeeded || len(completion.Steps) != 2 {
		t.Fatalf("resumed completion = %#v, %v", completion, err)
	}
	if calls[steps[0].PodName] != 0 || calls[steps[1].PodName] != 1 || strings.Contains(output.String(), "compile") || strings.Count(output.String(), "new log line") != 1 {
		t.Fatalf("duplicate replay: calls=%v output=%s", calls, output.String())
	}
	output.Reset()
	_, err = controller.WaitResumed(context.Background(), wf.Name, request.RunID, &output, []runprogress.Event{event(steps[0]), event(steps[1])}, 200, binding)
	if err != nil || output.Len() != 0 || calls[steps[1].PodName] != 1 {
		t.Fatalf("second resume replayed completed steps: %v %s %v", err, output.String(), calls)
	}
	output.Reset()
	completion, err = controller.WaitResumed(context.Background(), wf.Name, request.RunID, &output, nil, 4090, binding)
	if err == nil || output.Len() > 6 || completion.Name == "" {
		t.Fatalf("retained budget not enforced: output=%d completion=%#v err=%v", output.Len(), completion, err)
	}
}

func TestAdoptedWaitRefusesReplacedWorkflow(t *testing.T) {
	for _, mutation := range []string{"uid", "identity", "spec"} {
		wf, request := adoptionWorkflow()
		controller := &Controller{workflows: &staticWorkflowClient{workflow: wf}, config: Config{Namespace: wf.Namespace, MaxRunLogBytes: 4096}, interval: time.Millisecond}
		binding, _, err := controller.Adopt(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		switch mutation {
		case "uid":
			wf.UID = "replacement"
		case "identity":
			wf.Annotations[identityAnnotation] = strings.Repeat("c", 64)
		case "spec":
			wf.Spec.Templates[3].Container.Image = "foreign-image"
		}
		if _, err := controller.WaitResumed(context.Background(), wf.Name, request.RunID, io.Discard, nil, 0, binding); err == nil {
			t.Fatalf("Workflow replacement accepted: %s", mutation)
		}
	}
}

func TestAdoptionAcceptsActualServerBuiltWorkflow(t *testing.T) {
	request := testRequest(periapsis.TriggerCI, greedyDocument)
	workflow, err := Build(testConfig(), request)
	if err != nil {
		t.Fatal(err)
	}
	workflow.UID = "server-assigned-uid"
	controller := &Controller{workflows: &staticWorkflowClient{workflow: workflow}, config: testConfig()}
	binding, found, err := controller.Adopt(context.Background(), AdoptionRequest{Name: request.Name, RunID: request.RunID, Ref: request.Ref, SHA: request.SHA, Trigger: request.Trigger, RunTrigger: argoworkflow.RunTriggerBranch})
	if err != nil || !found || binding.Size != periapsis.L {
		t.Fatalf("built Workflow adoption = %#v %v %v", binding, found, err)
	}
}
