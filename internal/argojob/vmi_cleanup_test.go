package argojob

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

type countingVMIWorkflowClient struct {
	*staticWorkflowClient
	gets int
}

func (client *countingVMIWorkflowClient) Get(ctx context.Context, name string, options metav1.GetOptions) (*wfv1.Workflow, error) {
	client.gets++
	return client.staticWorkflowClient.Get(ctx, name, options)
}

func TestCleanupVMIsDeletesOnlyRunLabelAndNameWithUID(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{vmiResource: "VirtualMachineInstanceList"})
	name := "oberth-repo-run-abc"
	for _, tc := range []struct{ name, label string }{
		{name + "-amd64", name},
		{name + "-arm64", name},
		{"other-run-amd64", name},
		{name + "-foreign", "other-run"},
	} {
		object := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachineInstance"}}
		object.SetName(tc.name)
		object.SetNamespace(testNamespace)
		object.SetUID(types.UID(tc.name + "-uid"))
		object.SetLabels(map[string]string{"oberth.ci/workflow": tc.label})
		if _, err := client.Resource(vmiResource).Namespace(testNamespace).Create(ctx, object, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{{Name: "vm", Resource: &wfv1.ResourceTemplate{Action: "create"}}}}}
	workflow.Name = name
	workflow.Annotations = map[string]string{runIDAnnotation: "run-abc"}
	controller, err := NewController(&staticWorkflowClient{workflow: workflow}, nil, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	controller.WithVMIClient(client)
	if err := controller.CleanupVMIs(ctx, name, "run-abc"); err != nil {
		t.Fatal(err)
	}
	var deleted []string
	for _, action := range client.Actions() {
		if action.GetVerb() != "delete" {
			continue
		}
		deletion := action.(ktesting.DeleteAction)
		if options := deletion.GetDeleteOptions(); options.Preconditions == nil || options.Preconditions.UID == nil || string(*options.Preconditions.UID) != deletion.GetName()+"-uid" {
			t.Fatalf("delete lacks observed UID precondition: %+v", options)
		}
		deleted = append(deleted, deletion.GetName())
	}
	if len(deleted) != 2 {
		t.Fatalf("deleted VMIs = %v", deleted)
	}
	for _, foreign := range []string{"other-run-amd64", name + "-foreign"} {
		if _, err := client.Resource(vmiResource).Namespace(testNamespace).Get(ctx, foreign, metav1.GetOptions{}); err != nil {
			t.Fatalf("foreign VMI %s was deleted: %v", foreign, err)
		}
	}
	workflow.Spec.Templates = nil
	before := len(client.Actions())
	if err := controller.CleanupVMIs(ctx, name, "run-abc"); err != nil {
		t.Fatal(err)
	}
	if len(client.Actions()) != before {
		t.Fatal("ordinary workflow queried the VMI API")
	}
}

func TestTerminalStateSurvivesVMICleanupFailure(t *testing.T) {
	workflow := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "run-workflow", Namespace: testNamespace, UID: "wf-id", Annotations: map[string]string{runIDAnnotation: "run-abc"}}, Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{{Name: "vm", Resource: &wfv1.ResourceTemplate{Action: "create"}}}}, Status: wfv1.WorkflowStatus{Phase: wfv1.WorkflowSucceeded}}
	workflows := &countingVMIWorkflowClient{staticWorkflowClient: &staticWorkflowClient{workflow: workflow}}
	controller, err := NewController(workflows, nil, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{vmiResource: "VirtualMachineInstanceList"})
	client.PrependReactor("list", "virtualmachineinstances", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("virt-api unavailable")
	})
	controller.WithVMIClient(client)
	var observed error
	controller.WithVMICleanupFailure(func(_ context.Context, runID string, cleanupErr error) {
		if runID != "run-abc" {
			t.Fatalf("wrong audit run ID: %s", runID)
		}
		observed = cleanupErr
	})
	result, err := controller.TerminalState(t.Context(), workflow.Name)
	if err != nil || result == nil || !result.Succeeded {
		t.Fatalf("terminal workflow blocked by cleanup: %+v, %v", result, err)
	}
	if observed == nil || !strings.Contains(observed.Error(), "virt-api unavailable") {
		t.Fatal("cleanup failure not observed")
	}
	if workflows.gets != 1 || len(client.Actions()) != 1 {
		t.Fatalf("terminal cleanup made %d Workflow reads and %d VMI calls, want one each", workflows.gets, len(client.Actions()))
	}
}

func TestWaitSurvivesOneVMICleanupFailure(t *testing.T) {
	workflow := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "run-workflow", Namespace: testNamespace, UID: "wf-id", Annotations: map[string]string{runIDAnnotation: "run-abc"}}, Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{{Name: "vm", Resource: &wfv1.ResourceTemplate{Action: "create"}}}}, Status: wfv1.WorkflowStatus{Phase: wfv1.WorkflowSucceeded}}
	workflows := &countingVMIWorkflowClient{staticWorkflowClient: &staticWorkflowClient{workflow: workflow}}
	controller, err := NewController(workflows, nil, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{vmiResource: "VirtualMachineInstanceList"})
	client.PrependReactor("list", "virtualmachineinstances", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("virt-api unavailable")
	})
	controller.WithVMIClient(client)
	audits := 0
	controller.WithVMICleanupFailure(func(_ context.Context, runID string, cleanupErr error) {
		if runID != "run-abc" || !strings.Contains(cleanupErr.Error(), "virt-api unavailable") {
			t.Fatalf("wrong cleanup failure for %s: %v", runID, cleanupErr)
		}
		audits++
	})
	result, err := controller.Wait(t.Context(), workflow.Name, "run-abc", io.Discard)
	if err != nil || !result.Succeeded {
		t.Fatalf("terminal workflow blocked by cleanup: %+v, %v", result, err)
	}
	if audits != 1 || len(client.Actions()) != 1 || workflows.gets != 2 {
		t.Fatalf("cleanup audit=%d, VMI calls=%d, Workflow reads=%d; want 1, 1, 2", audits, len(client.Actions()), workflows.gets)
	}
}

type missingWorkflowClient struct{}

func (missingWorkflowClient) Create(context.Context, *wfv1.Workflow, metav1.CreateOptions) (*wfv1.Workflow, error) {
	return nil, errors.New("unexpected create")
}
func (missingWorkflowClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*wfv1.Workflow, error) {
	return nil, apierrors.NewNotFound(schema.GroupResource{Group: "argoproj.io", Resource: "workflows"}, name)
}
func (missingWorkflowClient) Delete(context.Context, string, metav1.DeleteOptions) error {
	return errors.New("unexpected delete")
}

func TestCancelMissingWorkflowToleratesVMIAccessForbidden(t *testing.T) {
	controller, err := NewController(missingWorkflowClient{}, nil, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{vmiResource: "VirtualMachineInstanceList"})
	client.PrependReactor("list", "virtualmachineinstances", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(vmiResource.GroupResource(), "", errors.New("not granted"))
	})
	controller.WithVMIClient(client)
	if err := controller.Cancel(t.Context(), "ordinary-run", "run-abc"); err != nil {
		t.Fatalf("ordinary run cancellation gated by VMI list: %v", err)
	}
}
