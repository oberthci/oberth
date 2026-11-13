package app

import (
	"fmt"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
)

// The production terminal reader must recognize native Argo controls after a
// restart, without inferring operator intent from a failure message alone.
func TestNativeOperatorShutdownTerminalMapping(t *testing.T) {
	for _, strategy := range []wfv1.ShutdownStrategy{wfv1.ShutdownStrategyStop, wfv1.ShutdownStrategyTerminate, "", "invalid"} {
		for _, phase := range []wfv1.WorkflowPhase{wfv1.WorkflowFailed, wfv1.WorkflowError, wfv1.WorkflowSucceeded} {
			t.Run(string(strategy)+"/"+string(phase), func(t *testing.T) {
				message := fmt.Sprintf("Stopped with strategy '%s'", strategy)
				wf := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "workflow", Namespace: "pipelines", UID: "workflow-uid", Annotations: map[string]string{"oberth.ci/run-id": "run"}}, Spec: wfv1.WorkflowSpec{Shutdown: strategy}, Status: wfv1.WorkflowStatus{Phase: phase, Message: message, FinishedAt: metav1.NewTime(time.Now())}}
				client := &targetWorkflowFake{object: wf}
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
					if result.Status != model.RunInterrupted || result.Phase != model.PhaseCancelled || result.Error != "stopped by operator: "+message || result.FailedStage != "" || result.FailedStep != "" {
						t.Fatalf("operator shutdown misclassified: %+v", result)
					}
				} else if result.Status == model.RunInterrupted || strings.HasPrefix(result.Error, "stopped by operator:") {
					t.Fatalf("invented operator intent: %+v", result)
				}
				if client.posts != 0 || client.deletes != 0 {
					t.Fatal("terminal observation mutated the Workflow")
				}
			})
		}
	}
}
