package argojob

import (
	"context"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

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
