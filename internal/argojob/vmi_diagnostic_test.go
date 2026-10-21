package argojob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestVMIFailureProjectionIsBoundedAndNonsecret(t *testing.T) {
	wf, _, _, at := diagnosticFixture()
	wf.Spec.Templates = []wfv1.Template{{Name: "vm", Resource: &wfv1.ResourceTemplate{Action: "create"}}}
	object := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachineInstance", "status": map[string]any{"phase": "Scheduling", "conditions": []any{map[string]any{"type": "Ready", "status": "False", "reason": "PodNotReady", "message": "secret-canary"}, map[string]any{"type": "secret-canary", "status": "False", "reason": "secret-canary"}}}}}
	object.SetName(wf.Name + "-vm")
	object.SetNamespace(wf.Namespace)
	object.SetUID("33333333-3333-4333-8333-333333333333")
	object.SetLabels(map[string]string{"oberth.ci/workflow": wf.Name})
	foreign := object.DeepCopy()
	foreign.SetName("other-vm")
	vmi := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{vmiResource: "VirtualMachineInstanceList"}, object, foreign)
	event := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "event", Namespace: wf.Namespace}, InvolvedObject: corev1.ObjectReference{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachineInstance", Namespace: wf.Namespace, Name: object.GetName(), UID: object.GetUID()}, Type: "Warning", Reason: "FailedScheduling", Message: "secret-canary", Count: 3, LastTimestamp: metav1.NewTime(at)}
	wrong := event.DeepCopy()
	wrong.Name = "wrong"
	wrong.InvolvedObject.UID = types.UID("other")
	wrong.Reason = "FailedMount"
	kube := fake.NewClientset(event, wrong)
	controller := &Controller{kube: kube, vmi: vmi}
	completion := completionFor(wf)
	var logs bytes.Buffer
	controller.collectVMIFailure(context.Background(), wf, &completion, &logs)
	if len(completion.VMIDiagnostics) == 0 || strings.Contains(string(completion.VMIDiagnostics), "secret-canary") || strings.Contains(logs.String(), "secret-canary") || !strings.Contains(logs.String(), "[test/test]") {
		t.Fatalf("invalid snapshot/log: %s / %s", completion.VMIDiagnostics, logs.String())
	}
	var value vmiSnapshot
	if err := json.Unmarshal(completion.VMIDiagnostics, &value); err != nil {
		t.Fatal(err)
	}
	if len(value.VMIs) != 1 || value.VMIs[0].Phase != "Scheduling" || len(value.VMIs[0].Conditions) != 2 || value.VMIs[0].Conditions[1].Reason != "other" || len(value.VMIs[0].Events) != 1 || value.VMIs[0].Events[0].Reason != "FailedScheduling" {
		t.Fatalf("projection %+v", value)
	}
	// Event reads are scoped by the exact VMI identity, regardless of what a
	// cache/fake/API returns; wrong-UID records never reach the projection.
	for _, action := range kube.Actions() {
		if action.GetVerb() == "list" {
			opts := action.(ktesting.ListActionImpl).GetListOptions()
			if opts.Limit != 64 || !strings.Contains(opts.FieldSelector, "involvedObject.uid="+string(object.GetUID())) {
				t.Fatal("unbounded Event read")
			}
		}
	}
	kube.PrependReactor("list", "events", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("secret-canary") })
	completion = completionFor(wf)
	controller.collectVMIFailure(context.Background(), wf, &completion, &logs)
	if strings.Contains(string(completion.VMIDiagnostics), "secret-canary") || !strings.Contains(string(completion.VMIDiagnostics), "observation-unavailable") {
		t.Fatal("Event read failure leaked or hidden")
	}
}
