package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

// The production terminal reader must recognize native Argo controls after a
// restart, without attributing who initiated them.
func TestNativeOperatorShutdownTerminalMapping(t *testing.T) {
	for _, strategy := range []wfv1.ShutdownStrategy{wfv1.ShutdownStrategyStop, wfv1.ShutdownStrategyTerminate, "", "invalid"} {
		for _, terminalStrategy := range []wfv1.ShutdownStrategy{wfv1.ShutdownStrategyStop, wfv1.ShutdownStrategyTerminate} {
			for _, phase := range []wfv1.WorkflowPhase{wfv1.WorkflowFailed, wfv1.WorkflowError, wfv1.WorkflowSucceeded} {
				t.Run(string(strategy)+"/"+string(terminalStrategy)+"/"+string(phase), func(t *testing.T) {
					message := fmt.Sprintf("Stopped with strategy '%s'", terminalStrategy)
					wf := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "workflow", Namespace: "pipelines", UID: "workflow-uid", Annotations: map[string]string{"oberth.ci/run-id": "run"}}, Spec: wfv1.WorkflowSpec{Shutdown: strategy}, Status: wfv1.WorkflowStatus{Phase: phase, Message: message, FinishedAt: metav1.NewTime(time.Now())}}
					client := &shutdownWorkflowFake{object: wf}
					controller, err := argojob.NewController(client, nil, argojob.Config{Namespace: "pipelines", PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "release", CISecretsServiceAccount: "ci", ExecutorServiceAccount: "executor"})
					if err != nil {
						t.Fatal(err)
					}
					jobs := &ArgoJobs{controller: controller}
					result, err := jobs.TerminalResult(t.Context(), wf.Name)
					if err != nil {
						t.Fatal(err)
					}
					stopped := phase != wfv1.WorkflowSucceeded && (strategy == wfv1.ShutdownStrategyStop || strategy == wfv1.ShutdownStrategyTerminate)
					if stopped {
						if result.Status != model.RunInterrupted || result.Phase != model.PhaseCancelled || result.Error != "Workflow stopped by its Workflow spec: "+message || result.FailedStage != "" || result.FailedStep != "" {
							t.Fatalf("native shutdown misclassified: %+v", result)
						}
					} else if result.Status == model.RunInterrupted || strings.HasPrefix(result.Error, "Workflow stopped:") {
						t.Fatalf("invented shutdown outcome: %+v", result)
					}
					if client.posts != 0 || client.deletes != 0 {
						t.Fatal("terminal observation mutated the Workflow")
					}
				})
			}
		}
	}
}

func TestAuthoredShutdownIsAdmittedSubmittedAndShownNeutrally(t *testing.T) {
	for _, strategy := range []wfv1.ShutdownStrategy{wfv1.ShutdownStrategyStop, wfv1.ShutdownStrategyTerminate} {
		t.Run(string(strategy), func(t *testing.T) {
			source := fmt.Sprintf(`apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  shutdown: %s
  templates:
  - name: main
    container:
      image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      command: [echo, ok]
`, strategy)
			authored, err := argoworkflow.Decode([]byte(source))
			if err != nil {
				t.Fatal(err)
			}
			if err = argoworkflow.Admit(authored, argoworkflow.Policy{}); err != nil {
				t.Fatal("native shutdown refused", err)
			}
			cfg := argojob.Config{Namespace: "pipelines", PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "release", CISecretsServiceAccount: "ci", ExecutorServiceAccount: "executor"}
			request := argojob.Request{RunID: strings.Repeat("a", 32), RepositoryID: 1, Name: "authored-shutdown", Repo: "example", Ref: "refs/heads/main", SHA: strings.Repeat("b", 40), Trigger: pipeline.TriggerCI, Source: []byte(source), ApprovedSecrets: map[string]bool{}}
			compiled, err := argojob.Build(cfg, request)
			if err != nil {
				t.Fatal(err)
			}
			client := &shutdownWorkflowFake{}
			posted, err := client.Create(t.Context(), compiled, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if authored.Spec.Shutdown != strategy || compiled.Spec.Shutdown != strategy || posted.Spec.Shutdown != strategy || client.posts != 1 {
				t.Fatal("authored shutdown changed before submission")
			}
			message := "Stopped with strategy '" + string(strategy) + "'"
			client.object.Status = wfv1.WorkflowStatus{Phase: wfv1.WorkflowFailed, Message: message, FinishedAt: metav1.NewTime(time.Now())}
			// A fresh controller and application have no in-memory submission state.
			controller, err := argojob.NewController(client, nil, cfg)
			if err != nil {
				t.Fatal(err)
			}
			binding, found, err := controller.Adopt(t.Context(), argojob.AdoptionRequest{RepositoryID: 1, Name: posted.Name, RunID: request.RunID, Ref: request.Ref, SHA: request.SHA, Trigger: request.Trigger, RunTrigger: argoworkflow.RunTriggerBranch})
			if err != nil || !found {
				t.Fatal("authored native shutdown could not be adopted", found, err)
			}
			if _, err = controller.TerminalStateResumed(t.Context(), posted.Name, request.RunID, binding, nil); err != nil {
				t.Fatal("resumed terminal read failed", err)
			}
			jobs := &ArgoJobs{controller: controller}
			result, err := jobs.TerminalResult(t.Context(), posted.Name)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != model.RunInterrupted || result.Phase != model.PhaseCancelled || result.Error != "Workflow stopped by its Workflow spec: "+message {
				t.Fatalf("authored shutdown attributed to an actor: %+v", result)
			}
			if client.posts != 1 || client.deletes != 0 || client.object.Spec.Shutdown != strategy {
				t.Fatal("restart observation changed the live Workflow")
			}
		})
	}
}

type shutdownWorkflowFake struct {
	object               *wfv1.Workflow
	posts, gets, deletes int
	err                  error
}

func (w *shutdownWorkflowFake) Create(_ context.Context, object *wfv1.Workflow, _ metav1.CreateOptions) (*wfv1.Workflow, error) {
	w.posts++
	if w.err != nil {
		return nil, w.err
	}
	copy := object.DeepCopy()
	copy.UID = "workflow-uid"
	copy.ResourceVersion = "1"
	w.object = copy
	return copy.DeepCopy(), nil
}
func (w *shutdownWorkflowFake) Get(context.Context, string, metav1.GetOptions) (*wfv1.Workflow, error) {
	w.gets++
	if w.err != nil {
		return nil, w.err
	}
	if w.object == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "workflows"}, "absent")
	}
	return w.object.DeepCopy(), nil
}
func (w *shutdownWorkflowFake) Delete(_ context.Context, _ string, options metav1.DeleteOptions) error {
	if w.object == nil {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "workflows"}, "absent")
	}
	if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != w.object.UID {
		return errors.New("missing Workflow UID fence")
	}
	w.deletes++
	w.object = nil
	return nil
}

func (*shutdownWorkflowFake) Patch(context.Context, string, types.PatchType, []byte, metav1.PatchOptions, ...string) (*wfv1.Workflow, error) {
	panic("terminal observation must not patch the Workflow")
}
