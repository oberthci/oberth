package argojob

import (
	"encoding/json"
	"errors"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	argofake "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

func TestCancelUsesNativeStopAndKeepsExitHandlers(t *testing.T) {
	for _, suspended := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "suspended"}[suspended], func(t *testing.T) {
			workflow := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "pipeline", UID: "workflow-uid", ResourceVersion: "1", Annotations: map[string]string{runIDAnnotation: "run"}}, Spec: wfv1.WorkflowSpec{Suspend: ptr.To(suspended), OnExit: "cleanup"}, Status: wfv1.WorkflowStatus{Phase: wfv1.WorkflowRunning}}
			client := argofake.NewSimpleClientset(workflow)
			controller := &Controller{workflows: client.ArgoprojV1alpha1().Workflows(workflow.Namespace)}
			for range 2 {
				if err := controller.Cancel(t.Context(), workflow.Name, "run"); err != nil {
					t.Fatal(err)
				}
			}
			observed, err := controller.workflows.Get(t.Context(), workflow.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if observed.Spec.Shutdown != wfv1.ShutdownStrategyStop || observed.Spec.OnExit != "cleanup" || observed.Spec.Suspend == nil || *observed.Spec.Suspend != suspended || observed.Status.Phase != wfv1.WorkflowRunning {
				t.Fatalf("Stop changed the graph, suspend or verdict: %+v", observed)
			}
			patches := 0
			for _, action := range client.Actions() {
				if action.GetResource().Resource != "workflows" {
					continue
				}
				if action.GetVerb() == "delete" {
					t.Fatal("Stop deleted a Workflow")
				}
				if action.GetVerb() == "patch" {
					patches++
					var body map[string]json.RawMessage
					if err := json.Unmarshal(action.(ktesting.PatchAction).GetPatch(), &body); err != nil {
						t.Fatal(err)
					}
					var metadata map[string]string
					if err := json.Unmarshal(body["metadata"], &metadata); err != nil {
						t.Fatal(err)
					}
					if metadata["uid"] != "workflow-uid" || metadata["resourceVersion"] != "" || string(body["spec"]) != `{"shutdown":"Stop"}` {
						t.Fatalf("patch lost its identity fence or rewrote a field: %s", action.(ktesting.PatchAction).GetPatch())
					}
				}
			}
			if patches != 1 {
				t.Fatalf("double Stop issued %d patches", patches)
			}
		})
	}
}

func TestCancelRefusesForeignOrReplacedWorkflow(t *testing.T) {
	for _, mode := range []string{"foreign", "replacement"} {
		t.Run(mode, func(t *testing.T) {
			workflow := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "pipeline", UID: "original", ResourceVersion: "1", Annotations: map[string]string{runIDAnnotation: "run"}}, Status: wfv1.WorkflowStatus{Phase: wfv1.WorkflowRunning}}
			if mode == "foreign" {
				workflow.Annotations[runIDAnnotation] = "foreign"
			}
			client := argofake.NewSimpleClientset(workflow)
			client.PrependReactor("patch", "workflows", func(ktesting.Action) (bool, runtime.Object, error) {
				replacement := workflow.DeepCopy()
				replacement.UID = "replacement"
				if err := client.Tracker().Update(wfv1.SchemeGroupVersion.WithResource("workflows"), replacement, workflow.Namespace); err != nil {
					t.Fatal(err)
				}
				return true, nil, apierrors.NewConflict(workflowTestResource, workflow.Name, errors.New("UID replaced"))
			})
			controller := &Controller{workflows: client.ArgoprojV1alpha1().Workflows(workflow.Namespace)}
			if err := controller.Cancel(t.Context(), "wf", "run"); err == nil {
				t.Fatal("foreign/replaced Workflow accepted")
			}
			for _, action := range client.Actions() {
				if action.GetVerb() == "delete" || mode == "foreign" && action.GetVerb() == "patch" {
					t.Fatal("foreign Workflow mutated")
				}
			}
			observed, err := controller.workflows.Get(t.Context(), "wf", metav1.GetOptions{})
			if err != nil || observed.Spec.Shutdown != "" {
				t.Fatal("replacement was stopped", err)
			}
		})
	}
}

func TestNativeTerminateSkipsExitHandlersAndCannotBeDowngraded(t *testing.T) {
	workflow := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "release", Namespace: "pipeline", UID: "original", ResourceVersion: "1", Annotations: map[string]string{runIDAnnotation: "run"}}, Spec: wfv1.WorkflowSpec{OnExit: "granted-publish"}, Status: wfv1.WorkflowStatus{Phase: wfv1.WorkflowRunning}}
	client := argofake.NewSimpleClientset(workflow)
	controller := &Controller{workflows: client.ArgoprojV1alpha1().Workflows(workflow.Namespace)}
	if err := controller.Cancel(t.Context(), workflow.Name, "run"); err != nil {
		t.Fatal(err)
	}
	if err := controller.Terminate(t.Context(), workflow.Name, "run"); err != nil {
		t.Fatal(err)
	}
	if err := controller.Cancel(t.Context(), workflow.Name, "run"); err != nil {
		t.Fatal(err)
	}
	observed, err := controller.workflows.Get(t.Context(), workflow.Name, metav1.GetOptions{})
	if err != nil || observed.Spec.Shutdown != wfv1.ShutdownStrategyTerminate || observed.Spec.OnExit != "granted-publish" {
		t.Fatalf("native immediate stop was weakened: %+v %v", observed, err)
	}
	patches := 0
	for _, action := range client.Actions() {
		if action.GetVerb() == "delete" {
			t.Fatal("shutdown deleted a Workflow")
		}
		if action.GetVerb() == "patch" {
			patches++
			var body struct {
				Metadata map[string]string
				Spec     map[string]string
			}
			if err := json.Unmarshal(action.(ktesting.PatchAction).GetPatch(), &body); err != nil {
				t.Fatal(err)
			}
			if len(body.Metadata) != 1 || body.Metadata["uid"] != "original" || len(body.Spec) != 1 {
				t.Fatalf("shutdown lost UID-only fence: %+v", body)
			}
		}
	}
	if patches != 2 {
		t.Fatalf("Stop, Terminate, Stop issued %d patches; want monotonic escalation", patches)
	}
}

func TestNativeShutdownRetriesConflictWithoutResourceVersionFence(t *testing.T) {
	workflow := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "workflow", Namespace: "pipeline", UID: "original", ResourceVersion: "1", Annotations: map[string]string{runIDAnnotation: "run"}}, Status: wfv1.WorkflowStatus{Phase: wfv1.WorkflowRunning}}
	client := argofake.NewSimpleClientset(workflow)
	patches := 0
	client.PrependReactor("patch", "workflows", func(ktesting.Action) (bool, runtime.Object, error) {
		patches++
		if patches == 1 {
			return true, nil, apierrors.NewConflict(workflowTestResource, workflow.Name, errors.New("status changed"))
		}
		return false, nil, nil
	})
	controller := &Controller{workflows: client.ArgoprojV1alpha1().Workflows(workflow.Namespace)}
	if err := controller.Terminate(t.Context(), workflow.Name, "run"); err != nil {
		t.Fatal(err)
	}
	if patches != 2 {
		t.Fatalf("conflict attempts=%d", patches)
	}
}
