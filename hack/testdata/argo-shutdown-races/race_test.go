package shutdownraces

import (
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	argofake "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned/fake"
	logging "github.com/argoproj/argo-workflows/v4/util/logging"
	argoutil "github.com/argoproj/argo-workflows/v4/workflow/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"

	"context"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
)

func TestReviewerStopRacePreservesGenuineFailure(t *testing.T) {
	for _, strategy := range []wfv1.ShutdownStrategy{wfv1.ShutdownStrategyStop, wfv1.ShutdownStrategyTerminate} {
		t.Run(string(strategy), func(t *testing.T) {
			wf := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "race", Namespace: "pipelines", UID: "original", Annotations: map[string]string{"oberth.ci/run-id": "run"}}, Status: wfv1.WorkflowStatus{Phase: wfv1.WorkflowRunning}}
			wf.Spec = wfv1.WorkflowSpec{Entrypoint: "ci", Templates: []wfv1.Template{
				{Name: "ci", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "main", Template: "main"}}}},
				{Name: "main", Container: &corev1.Container{Image: "alpine"}},
			}}
			client := argofake.NewSimpleClientset(wf)
			raced := false
			client.PrependReactor("get", "workflows", func(a ktesting.Action) (bool, runtime.Object, error) {
				if raced {
					return false, nil, nil
				}
				raced = true
				ended := wf.DeepCopy()
				ended.Status.Phase = wfv1.WorkflowFailed
				ended.Status.Message = "main failed: exit code 23"
				ended.Status.FinishedAt = metav1.NewTime(time.Now())
				ended.Status.Nodes = wfv1.Nodes{
					"root": {ID: "root", Name: wf.Name, Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeFailed},
					"main": {ID: "main", Name: wf.Name + ".main", DisplayName: "main", TemplateName: "main", BoundaryID: "root", Type: wfv1.NodeTypePod, Phase: wfv1.NodeFailed, Message: ended.Status.Message, StartedAt: metav1.NewTime(time.Now().Add(-time.Second)), FinishedAt: ended.Status.FinishedAt},
				}
				ended.Labels = map[string]string{"workflows.argoproj.io/completed": "true"}
				if err := client.Tracker().Update(wfv1.SchemeGroupVersion.WithResource("workflows"), ended, wf.Namespace); err != nil {
					t.Fatal(err)
				}
				// Return the GET snapshot taken just before the terminal update. Pinned
				// Argo util then sends its merge patch without a resourceVersion guard.
				return true, wf.DeepCopy(), nil
			})
			wfc := client.ArgoprojV1alpha1().Workflows(wf.Namespace)
			var err error
			if strategy == wfv1.ShutdownStrategyStop {
				err = argoutil.StopWorkflow(logging.TestContext(t.Context()), wfc, nil, wf.Name, "", "")
			} else {
				err = argoutil.TerminateWorkflow(logging.TestContext(t.Context()), wfc, wf.Name)
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := wfc.Get(t.Context(), wf.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got.Status.Message != "main failed: exit code 23" || got.Spec.Shutdown != strategy {
				t.Fatal("race not exercised", got)
			}
			c, err := argojob.NewController(wfc, nil, argojob.Config{Namespace: wf.Namespace, PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "release", CISecretsServiceAccount: "ci", ExecutorServiceAccount: "executor"})
			if err != nil {
				t.Fatal(err)
			}
			jobs, err := app.NewArgoJobs(c, argojob.Config{Namespace: wf.Namespace, PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "release", CISecretsServiceAccount: "ci", ExecutorServiceAccount: "executor"}, unusedAuditor{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			result, err := jobs.TerminalResult(t.Context(), wf.Name)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("pinned Argo patch after terminal failure: status=%s phase=%s error=%q", result.Status, result.Phase, result.Error)
			if result.Status != model.RunFailed || result.FailedStage != "main" || result.FailedStep != "main" || result.Error != got.Status.Message || strings.Contains(result.Error, "stopped by operator") {
				t.Fatalf("genuine terminal failure mislabeled after late %s patch: %+v", strategy, result)
			}
		})
	}
}

func TestReviewerShutdownEscalationKeepsCompletedStop(t *testing.T) {
	for _, strategy := range []wfv1.ShutdownStrategy{wfv1.ShutdownStrategyStop, wfv1.ShutdownStrategyTerminate} {
		t.Run(string(strategy), func(t *testing.T) {
			wf := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "race", Namespace: "pipelines", UID: "original", Annotations: map[string]string{"oberth.ci/run-id": "run"}}, Status: wfv1.WorkflowStatus{Phase: wfv1.WorkflowRunning}}
			completedStrategy := wfv1.ShutdownStrategyStop
			if strategy == completedStrategy {
				completedStrategy = wfv1.ShutdownStrategyTerminate
			}
			wf.Spec.Shutdown = completedStrategy
			stoppedMessage := "Stopped with strategy '" + string(completedStrategy) + "'"
			client := argofake.NewSimpleClientset(wf)
			raced := false
			client.PrependReactor("get", "workflows", func(a ktesting.Action) (bool, runtime.Object, error) {
				if raced {
					return false, nil, nil
				}
				raced = true
				ended := wf.DeepCopy()
				ended.Status.Phase = wfv1.WorkflowFailed
				ended.Status.Message = stoppedMessage
				ended.Status.FinishedAt = metav1.NewTime(time.Now())
				ended.Labels = map[string]string{"workflows.argoproj.io/completed": "true"}
				if err := client.Tracker().Update(wfv1.SchemeGroupVersion.WithResource("workflows"), ended, wf.Namespace); err != nil {
					t.Fatal(err)
				}
				// Return the GET snapshot taken just before the terminal update. Pinned
				// Argo util then sends its merge patch without a resourceVersion guard.
				return true, wf.DeepCopy(), nil
			})
			wfc := client.ArgoprojV1alpha1().Workflows(wf.Namespace)
			var err error
			if strategy == wfv1.ShutdownStrategyStop {
				err = argoutil.StopWorkflow(logging.TestContext(t.Context()), wfc, nil, wf.Name, "", "")
			} else {
				err = argoutil.TerminateWorkflow(logging.TestContext(t.Context()), wfc, wf.Name)
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := wfc.Get(t.Context(), wf.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got.Status.Message != stoppedMessage || got.Spec.Shutdown != strategy {
				t.Fatal("race not exercised", got)
			}
			c, err := argojob.NewController(wfc, nil, argojob.Config{Namespace: wf.Namespace, PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "release", CISecretsServiceAccount: "ci", ExecutorServiceAccount: "executor"})
			if err != nil {
				t.Fatal(err)
			}
			jobs, err := app.NewArgoJobs(c, argojob.Config{Namespace: wf.Namespace, PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "release", CISecretsServiceAccount: "ci", ExecutorServiceAccount: "executor"}, unusedAuditor{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			result, err := jobs.TerminalResult(t.Context(), wf.Name)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("pinned Argo strategy change after real stop completed: status=%s phase=%s error=%q", result.Status, result.Phase, result.Error)
			if result.Status != model.RunInterrupted || result.Phase != model.PhaseCancelled || result.Error != "stopped by operator: "+stoppedMessage {
				t.Fatalf("completed operator stop mislabeled after late %s patch: %+v", strategy, result)
			}
		})
	}
}

// These terminal readers must not need submission audit writes.
type unusedAuditor struct{}

func (unusedAuditor) AppendAuditAction(context.Context, model.AuditActionSpec) (model.AuditAction, error) {
	panic("terminal observation attempted an audit mutation")
}
