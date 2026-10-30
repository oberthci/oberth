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
		{name + "-foreign-owner", name},
	} {
		object := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachineInstance"}}
		object.SetName(tc.name)
		object.SetNamespace(testNamespace)
		object.SetUID(types.UID(tc.name + "-uid"))
		object.SetLabels(map[string]string{"oberth.ci/workflow": tc.label})
		object.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: name, UID: "wf-id"}})
		if strings.HasSuffix(tc.name, "-foreign-owner") {
			object.SetOwnerReferences(nil)
		}
		if _, err := client.Resource(vmiResource).Namespace(testNamespace).Create(ctx, object, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{{Name: "vm", Resource: &wfv1.ResourceTemplate{Action: "create"}}}}}
	workflow.Name = name
	workflow.Namespace = testNamespace
	workflow.UID = "wf-id"
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
	for _, foreign := range []string{"other-run-amd64", name + "-foreign", name + "-foreign-owner"} {
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

// failedVMIWorkflow is a run whose create step failed before KubeVirt
// accepted the VMI. It declares no delete step: the cleanup backstop owns
// deletion (#1017), so workflows need no exit-handler delete.
func failedVMIWorkflow() *wfv1.Workflow {
	return &wfv1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "run-workflow", Namespace: testNamespace, UID: "wf-id", Annotations: map[string]string{runIDAnnotation: "run-abc"}},
		Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{
			{Name: "vm-create", Resource: &wfv1.ResourceTemplate{Action: "create"}},
			{Name: "vm-get", Resource: &wfv1.ResourceTemplate{Action: "get"}},
		}},
		Status: wfv1.WorkflowStatus{Phase: wfv1.WorkflowFailed, Message: "child 'vm-create' failed"},
	}
}

func newVMIClient(t *testing.T, names ...string) *fake.FakeDynamicClient {
	t.Helper()
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{vmiResource: "VirtualMachineInstanceList"})
	for _, name := range names {
		object := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachineInstance"}}
		object.SetName(name)
		object.SetNamespace(testNamespace)
		object.SetUID(types.UID(name + "-uid"))
		object.SetLabels(map[string]string{"oberth.ci/workflow": "run-workflow"})
		object.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: "run-workflow", UID: "wf-id"}})
		if _, err := client.Resource(vmiResource).Namespace(testNamespace).Create(t.Context(), object, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	client.ClearActions()
	return client
}

func TestWaitRunFailedBeforeVMICreateCleansUpCleanly(t *testing.T) {
	workflow := failedVMIWorkflow()
	controller, err := NewController(&staticWorkflowClient{workflow: workflow}, nil, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	client := newVMIClient(t)
	controller.WithVMIClient(client)
	controller.WithVMICleanupFailure(func(_ context.Context, runID string, cleanupErr error) {
		t.Fatalf("cleanup of a run with no VMI reported a failure for %s: %v", runID, cleanupErr)
	})
	result, err := controller.Wait(t.Context(), workflow.Name, "run-abc", io.Discard)
	if err != nil || result.Succeeded {
		t.Fatalf("failed run = %+v, %v; want a failed completion without a cleanup error", result, err)
	}
	var verbs []string
	for _, action := range client.Actions() {
		verbs = append(verbs, action.GetVerb())
	}
	if strings.Join(verbs, ",") != "list" {
		t.Fatalf("VMI calls = %v, want one list and no delete", verbs)
	}
}

func TestWaitBackstopDeletesVMIsWithoutDeleteStep(t *testing.T) {
	workflow := failedVMIWorkflow()
	workflow.Status.Message = "child 'driver' failed"
	controller, err := NewController(&staticWorkflowClient{workflow: workflow}, nil, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	client := newVMIClient(t, "run-workflow-p2p0", "run-workflow-p2p1")
	// One VMI disappears between the list and its delete (the owner
	// reference or an exit-handler delete won the race): not an error.
	client.PrependReactor("delete", "virtualmachineinstances", func(action ktesting.Action) (bool, runtime.Object, error) {
		if action.(ktesting.DeleteAction).GetName() == "run-workflow-p2p1" {
			return true, nil, apierrors.NewNotFound(vmiResource.GroupResource(), "run-workflow-p2p1")
		}
		return false, nil, nil
	})
	controller.WithVMIClient(client)
	controller.WithVMICleanupFailure(func(_ context.Context, runID string, cleanupErr error) {
		t.Fatalf("backstop reported a failure for %s: %v", runID, cleanupErr)
	})
	if _, err := controller.Wait(t.Context(), workflow.Name, "run-abc", io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Resource(vmiResource).Namespace(testNamespace).Get(t.Context(), "run-workflow-p2p0", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("leftover VMI survived the backstop: %v", err)
	}
	deletes := 0
	for _, action := range client.Actions() {
		if action.GetVerb() == "delete" {
			deletes++
		}
	}
	if deletes != 2 {
		t.Fatalf("backstop deletes = %d, want one per listed VMI", deletes)
	}
}
