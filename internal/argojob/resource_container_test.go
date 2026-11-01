package argojob

import (
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

func TestResourceLauncherRequiresWorkflowVMIPodUIDChain(t *testing.T) {
	for _, bad := range []string{"", "workflow-owner", "operation-pod", "launcher-owner", "launcher-kind", "launcher-name"} {
		t.Run(bad, func(t *testing.T) {
			wf, _, pod, at := diagnosticFixture()
			pod.Spec.NodeName = "node-a"
			wf.Spec.Templates = append(wf.Spec.Templates, wfv1.Template{Name: "vm", Resource: &wfv1.ResourceTemplate{Manifest: "kind: VirtualMachineInstance"}})
			uid := types.UID("33333333-3333-4333-8333-333333333333")
			vmi := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachineInstance", "metadata": map[string]any{"name": wf.Name + "-vm", "namespace": wf.Namespace, "uid": string(uid), "annotations": map[string]any{vmiOperationAnnotation: string(pod.UID)}}}}
			vmi.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: wf.Name, UID: wf.UID, Controller: ptr.To(true)}})
			launcher := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "virt-launcher-test", Namespace: wf.Namespace, UID: "44444444-4444-4444-8444-444444444444", Labels: map[string]string{"kubevirt.io": "virt-launcher"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachineInstance", Name: vmi.GetName(), UID: uid, Controller: ptr.To(true)}}}, Spec: corev1.PodSpec{NodeName: "node-a", Containers: []corev1.Container{{Name: "compute"}}}, Status: corev1.PodStatus{StartTime: ptr.To(metav1.NewTime(at.Add(-time.Minute))), ContainerStatuses: []corev1.ContainerStatus{{Name: "compute", ContainerID: "containerd://" + strings.Repeat("c", 64), State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(at.Add(-time.Minute))}}}}}}
			switch bad {
			case "workflow-owner":
				refs := vmi.GetOwnerReferences()
				refs[0].UID = "foreign"
				vmi.SetOwnerReferences(refs)
			case "operation-pod":
				vmi.SetAnnotations(map[string]string{vmiOperationAnnotation: "foreign"})
			case "launcher-owner":
				launcher.OwnerReferences[0].UID = "foreign"
			case "launcher-kind":
				launcher.OwnerReferences[0].Kind = "Job"
			case "launcher-name":
				launcher.OwnerReferences[0].Name = "foreign"
			}
			dynamic := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{vmiResource: "VirtualMachineInstanceList"}, vmi)
			kube := fake.NewClientset(pod, launcher)
			source, recorder := &resourceSource{fail: true}, &resourceRecorder{}
			c := &Controller{kube: kube, vmi: dynamic, config: Config{Namespace: wf.Namespace}, resourceSource: source, resourceRecorder: recorder}
			if err := c.sampleResourceUsage(t.Context(), wf, wf.Annotations[runIDAnnotation]); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, v := range recorder.values {
				if v.PodKind == "vm-launcher" {
					found = true
					if v.ContainerName != "compute" || v.MemoryPeakBytes != nil || v.Samples != 0 || v.PodStartedAt.IsZero() {
						t.Fatal("bad launcher timing", v)
					}
				}
			}
			if found != (bad == "") {
				t.Fatal("owner chain not enforced", bad, recorder.values)
			}
			if source.calls != 1 {
				t.Fatal("node snapshot not shared", source.calls)
			}
			for _, action := range append(kube.Actions(), dynamic.Actions()...) {
				if action.GetVerb() != "list" {
					t.Fatal("sampler mutated cluster", action)
				}
			}
		})
	}
}
