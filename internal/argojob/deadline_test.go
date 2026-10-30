package argojob

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestWorkflowDeadlineSubmittedAndRetryPreservesClock(t *testing.T) {
	for _, test := range []struct {
		name, seconds string
		want          int64
	}{{"declared", "5400", 5400}, {"ceiling", "999999", 7200}} {
		t.Run(test.name, func(t *testing.T) {
			config := seedTestConfig()
			kube := fake.NewClientset()
			runningSeedPods(kube)
			workflows := &nonrootWorkflowClient{}
			controller, err := NewController(workflows, kube, config)
			if err != nil {
				t.Fatal(err)
			}
			var calls []seedExecCall
			controller.WithSourceSeeder(NewSourceSeeder(kube, recordingExec(&calls), config))
			source := strings.Replace(greedyDocument, "activeDeadlineSeconds: 999999", "activeDeadlineSeconds: "+test.seconds, 1)
			source = strings.Replace(source, "    - name: main\n", "    - name: main\n      activeDeadlineSeconds: 60\n      timeout: 75s\n", 1)
			request := testRequest(periapsis.TriggerCI, source)
			request.SourceDir = writeCheckout(t)
			if _, err := controller.Create(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			got := workflows.existing // Captured at the API Create boundary, JSON round-tripped.
			if got.Spec.ActiveDeadlineSeconds == nil || *got.Spec.ActiveDeadlineSeconds != test.want {
				t.Fatalf("submitted deadline = %v, want %d", got.Spec.ActiveDeadlineSeconds, test.want)
			}
			leaf := got.Spec.Templates[0]
			if leaf.ActiveDeadlineSeconds == nil || leaf.ActiveDeadlineSeconds.IntVal != 60 || leaf.Timeout != "75s" {
				t.Fatal("submission lost the shorter template deadlines")
			}
			created := got.CreationTimestamp
			seeded := len(calls)
			if _, err := controller.Create(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			if workflows.creates != 1 || len(calls) != seeded || !created.Equal(&workflows.existing.CreationTimestamp) {
				t.Fatal("idempotent retry recreated/reseeded the Workflow or reset its clock")
			}
		})
	}
}

func TestWorkflowDeadlineObservationClockAndLag(t *testing.T) {
	started := time.Date(2026, 10, 30, 9, 3, 37, 0, time.UTC)
	workflow := schedulingWorkflow()
	workflow.Spec.ActiveDeadlineSeconds = ptr.To(int64(5400))
	workflow.CreationTimestamp = metav1.NewTime(started.Add(-time.Minute))
	workflow.Status.StartedAt = metav1.NewTime(started)
	result := &model.ExecutionObservation{State: "observed"}
	observeWorkflowDeadline(workflow, result, started.Add(time.Hour))
	if result.Deadline.Clock != "workflow_started_at" || *result.Deadline.RemainingSeconds != 1800 || !result.Deadline.ExpiresAt.Equal(started.Add(90*time.Minute)) {
		t.Fatalf("wrong clock (admission/creation must not replace Argo start): %+v", result.Deadline)
	}
	observeWorkflowDeadline(workflow, result, started.Add(91*time.Minute))
	if result.State != "deadline_elapsed" || result.Reason != "" || *result.Deadline.RemainingSeconds != 0 {
		t.Fatalf("clock alone claimed enforcement: %+v", result)
	}
	workflow.Status.Phase = wfv1.WorkflowFailed
	workflow.Status.Message = "Step exceeded its deadline"
	workflow.Status.FinishedAt = metav1.NewTime(time.Date(2026, 10, 30, 10, 33, 40, 0, time.UTC))
	observeWorkflowDeadline(workflow, result, time.Date(2026, 10, 30, 11, 25, 0, 0, time.UTC))
	if result.State != "deadline_exceeded" || result.Reason != "DeadlineExceeded" || *result.ReconciliationLagSeconds != 3080 || result.ObservedAt.IsZero() {
		t.Fatalf("missing deadline/lag evidence: %+v", result)
	}
	if completion := completionFor(workflow); completion.Succeeded || completion.Reason != model.ExecutionDeadlineMessage {
		t.Fatalf("terminal deadline cause discarded: %+v", completion)
	}
	workflow.Status.Phase, workflow.Status.Message = wfv1.WorkflowRunning, ""
	workflow.Status.StartedAt = metav1.Time{}
	result = &model.ExecutionObservation{State: "observed"}
	observeWorkflowDeadline(workflow, result, started)
	if result.Deadline.ExpiresAt != nil || result.Deadline.RemainingSeconds != nil {
		t.Fatal("unknown Workflow start was replaced by creation or admission")
	}
}

func TestWorkflowDeadlineObservationRequiresEvidence(t *testing.T) {
	workflow := schedulingWorkflow()
	workflow.Status.Phase = wfv1.WorkflowFailed
	workflow.Status.Nodes["test"] = wfv1.NodeStatus{Phase: wfv1.NodeFailed, Message: "exit status 143", Outputs: &wfv1.Outputs{ExitCode: ptr.To("143")}}
	result := &model.ExecutionObservation{}
	observeWorkflowDeadline(workflow, result, time.Now())
	if result.State == "deadline_exceeded" || completionFor(workflow).Reason == model.ExecutionDeadlineMessage {
		t.Fatal("exit 143 alone was mistaken for a deadline")
	}
	workflow = budgetTestWorkflow()
	workflow.Status.Phase = wfv1.WorkflowFailed
	failed := workflow.Status.Nodes["unit-pod"]
	failed.Phase, failed.Message = wfv1.NodeFailed, "Step exceeded its deadline"
	workflow.Status.Nodes["unit-pod"] = failed
	if completionFor(workflow).Reason != model.ExecutionDeadlineMessage {
		t.Fatal("node deadline reason was lost")
	}
}

func TestWorkflowDeadlineObservationOnlyUsesOwnedPods(t *testing.T) {
	workflow := schedulingWorkflow()
	workflow.Status.Phase = wfv1.WorkflowFailed
	fixture := budgetTestWorkflow()
	workflow.Spec, workflow.Status.Nodes = fixture.Spec, fixture.Status.Nodes
	failed := workflow.Status.Nodes["unit-pod"]
	failed.Phase, failed.Message = wfv1.NodeFailed, "Step exceeded its deadline"
	workflow.Status.Nodes["unit-pod"] = failed
	// Keep the node generic initially so only the correctly owned Pod supplies
	// the deadline reason; its current-attempt identity must also match.
	failed.Message = "exit status 143"
	workflow.Status.Nodes["unit-pod"] = failed
	pod := schedulingPod("deadline", "foreign")
	pod.Annotations = map[string]string{"workflows.argoproj.io/node-id": "unit-pod"}
	pod.Status.Phase, pod.Status.Reason, pod.Status.Message = corev1.PodFailed, "DeadlineExceeded", "PRIVATE controller text"
	kube := fake.NewClientset(pod)
	controller := &Controller{workflows: &schedulingWorkflowClient{workflow: workflow}, kube: kube, config: Config{Namespace: "pipeline"}}
	result, err := controller.ObserveScheduling(t.Context(), "wf", "run", "sha")
	if err != nil || result.Reason == "DeadlineExceeded" {
		t.Fatalf("foreign Pod influenced status: %+v %v", result, err)
	}
	pod.OwnerReferences[0].UID = workflow.UID
	controller.kube = fake.NewClientset(pod)
	result, err = controller.ObserveScheduling(t.Context(), "wf", "run", "sha")
	if err != nil || result.State != "deadline_exceeded" {
		t.Fatalf("owned deadline not visible: %+v %v", result, err)
	}
	body, _ := json.Marshal(result)
	if strings.Contains(string(body), "PRIVATE") {
		t.Fatal("free-form controller message leaked")
	}
	pod.Annotations["workflows.argoproj.io/node-id"] = "earlier-attempt"
	controller.kube = fake.NewClientset(pod)
	result, err = controller.ObserveScheduling(t.Context(), "wf", "run", "sha")
	if err != nil || result.State == "deadline_exceeded" {
		t.Fatal("an earlier attempt's Pod supplied the current failure reason")
	}
	pod.Annotations["workflows.argoproj.io/node-id"] = "unit-pod"
	controller.kube = fake.NewClientset(pod)
	workflow.Status.Phase = wfv1.WorkflowSucceeded
	workflow.Status.Message = "Step exceeded its deadline"
	result, err = controller.ObserveScheduling(t.Context(), "wf", "run", "sha")
	if err != nil || result.State == "deadline_exceeded" || workflowDeadlineExceeded(workflow) {
		t.Fatal("a successful retry inherited an earlier attempt's deadline failure")
	}
}
