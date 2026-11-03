package argojob

import (
	"context"
	"errors"
	"fmt"
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

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/resourcesampler"
)

func TestResourceLauncherRequiresWorkflowVMIPodUIDChain(t *testing.T) {
	for _, bad := range []string{"", "workflow-owner", "operation-pod", "launcher-owner", "launcher-kind", "launcher-name"} {
		t.Run(bad, func(t *testing.T) {
			wf, _, pod, at := diagnosticFixture()
			pod.Spec.NodeName = "node-a"
			wf.Spec.Templates = append(wf.Spec.Templates, wfv1.Template{Name: "vm", Resource: &wfv1.ResourceTemplate{Manifest: "kind: VirtualMachineInstance"}})
			uid := types.UID("33333333-3333-4333-8333-333333333333")
			vmi := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachineInstance", "metadata": map[string]any{"name": wf.Name + "-vm", "namespace": wf.Namespace, "uid": string(uid), "annotations": map[string]any{vmiOperationAnnotation: string(pod.UID)}}}}
			vmi.SetLabels(map[string]string{"oberth.ci/workflow": wf.Name})
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

type retryResourceRecorder struct {
	resourceRecorder
	fail  bool
	calls int
}

func (r *retryResourceRecorder) RecordStepResourceUsage(ctx context.Context, values []model.StepResourceUsage) error {
	r.calls++
	if r.fail {
		return errors.New("unavailable")
	}
	return r.resourceRecorder.RecordStepResourceUsage(ctx, values)
}

func TestResourceSessionSkipsFinishedOnlyAfterSuccessfulRecording(t *testing.T) {
	wf, _, pod, _ := diagnosticFixture()
	pod.Spec.NodeName = "node-a"
	source, recorder := &resourceSource{}, &retryResourceRecorder{fail: true}
	c := &Controller{kube: fake.NewClientset(pod), resourceSource: source, resourceRecorder: recorder}
	finished := map[string]time.Time{}
	if err := c.sampleResourceUsageWithState(t.Context(), wf, wf.Annotations[runIDAnnotation], finished); err == nil || len(finished) != 0 {
		t.Fatal("failed recording suppressed final evidence", err, finished)
	}
	recorder.fail = false
	if err := c.sampleResourceUsageWithState(t.Context(), wf, wf.Annotations[runIDAnnotation], finished); err != nil || len(finished) != 1 {
		t.Fatal("finish not cached", err, finished)
	}
	if err := c.sampleResourceUsageWithState(t.Context(), wf, wf.Annotations[runIDAnnotation], finished); err != nil {
		t.Fatal(err)
	}
	if recorder.calls != 2 || source.calls != 2 || len(recorder.values) != 1 {
		t.Fatal("finished containers reached recorder/scraper again", recorder.calls, source.calls)
	}
	// A long run may have more lifetime IDs than durable row capacity. The
	// session's suppression set must stay bounded across later passes too.
	finished = map[string]time.Time{}
	for i := 0; i < 4096; i++ {
		finished[fmt.Sprint(i)] = time.Time{}
	}
	if err := c.sampleResourceUsageWithState(t.Context(), wf, wf.Annotations[runIDAnnotation], finished); err != nil || len(finished) != 4096 {
		t.Fatal("finished-ID cache exceeded bound", len(finished), err)
	}
}

type panicResourceSource struct{}

func (panicResourceSource) Interval() time.Duration { return time.Second }
func (panicResourceSource) Snapshot(context.Context, string) (resourcesampler.Snapshot, error) {
	panic("sampler panic canary")
}

func TestResourceSessionContainsTelemetryPanic(t *testing.T) {
	wf, _, pod, _ := diagnosticFixture()
	pod.Spec.NodeName = "node-a"
	c := &Controller{kube: fake.NewClientset(pod), resourceSource: panicResourceSource{}, resourceRecorder: &resourceRecorder{}}
	session := c.resourceSession(t.Context(), wf.Annotations[runIDAnnotation])
	session.update(wf)
	select {
	case <-session.done:
	case <-time.After(5 * time.Second):
		t.Fatal("panicking sampler did not stop/join")
	}
	session.finish()
}

func TestResourceWorkflowWithoutVMDoesNotInventoryVMIs(t *testing.T) {
	wf, _, pod, _ := diagnosticFixture()
	pod.Spec.NodeName = "node-a"
	dynamic := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{vmiResource: "VirtualMachineInstanceList"})
	c := &Controller{kube: fake.NewClientset(pod), vmi: dynamic, resourceSource: &resourceSource{}, resourceRecorder: &resourceRecorder{}}
	if err := c.sampleResourceUsage(t.Context(), wf, wf.Annotations[runIDAnnotation]); err != nil {
		t.Fatal(err)
	}
	if len(dynamic.Actions()) != 0 {
		t.Fatal("ordinary workflow listed VMIs", dynamic.Actions())
	}
}

func TestResourceCandidateCapsWorkflowAndLauncherPasses(t *testing.T) {
	for _, launcherMode := range []bool{false, true} {
		t.Run(fmt.Sprint("launcher=", launcherMode), func(t *testing.T) {
			wf, node, base, at := diagnosticFixture()
			base.Spec.NodeName = "node-a"
			objects := []runtime.Object{}
			var vmis []runtime.Object
			if launcherMode {
				objects = append(objects, base)
				wf.Spec.Templates = []wfv1.Template{{Name: "vm", Resource: &wfv1.ResourceTemplate{Manifest: "kind: VirtualMachineInstance"}}}
				vmi := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachineInstance", "metadata": map[string]any{"name": "vm-test", "namespace": wf.Namespace, "uid": "33333333-3333-4333-8333-333333333333", "labels": map[string]any{"oberth.ci/workflow": wf.Name}, "annotations": map[string]any{vmiOperationAnnotation: string(base.UID)}}}}
				vmi.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: wf.Name, UID: wf.UID, Controller: ptr.To(true)}})
				vmis = append(vmis, vmi)
			} else {
				wf.Status.Nodes = wfv1.Nodes{}
			}
			for i := 0; i < 33; i++ {
				p := base.DeepCopy()
				p.UID = types.UID(fmt.Sprintf("%08x-2222-4222-8222-222222222222", i+1))
				p.Spec.Containers = nil
				p.Status.ContainerStatuses = nil
				if launcherMode {
					p.Name = fmt.Sprintf("launcher-%d", i)
					p.Labels = map[string]string{"kubevirt.io": "virt-launcher"}
					p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachineInstance", Name: "vm-test", UID: "33333333-3333-4333-8333-333333333333", Controller: ptr.To(true)}}
				} else {
					n := node
					n.ID = fmt.Sprintf("workflow-test-%d", i)
					n.Name = fmt.Sprintf("workflow-test.step%d", i)
					n.DisplayName = fmt.Sprintf("step%d", i)
					wf.Status.Nodes[n.ID] = n
					p.Name = PodName(wf, n)
					p.Annotations = map[string]string{"workflows.argoproj.io/node-id": n.ID, "workflows.argoproj.io/node-name": n.Name}
				}
				for j := 0; j < 128; j++ {
					name := fmt.Sprintf("c%d", j)
					p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: name})
					p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, corev1.ContainerStatus{Name: name, ContainerID: fmt.Sprintf("containerd://%064x", i*128+j+1), State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(at.Add(-time.Minute))}}})
				}
				objects = append(objects, p)
			}
			dynamic := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{vmiResource: "VirtualMachineInstanceList"}, vmis...)
			source, recorder := &resourceSource{}, &resourceRecorder{}
			c := &Controller{kube: fake.NewClientset(objects...), vmi: dynamic, resourceSource: source, resourceRecorder: recorder}
			if err := c.sampleResourceUsage(t.Context(), wf, wf.Annotations[runIDAnnotation]); err != nil {
				t.Fatal(err)
			}
			if len(recorder.values) < 3500 || len(recorder.values) > 4096 {
				t.Fatal("candidate cap missing or fixture not exercised", len(recorder.values))
			}
			if source.calls != 1 {
				t.Fatal("node reads not coalesced", source.calls)
			}
		})
	}
}

func TestResourceSessionRecordsLatePodFinishOnce(t *testing.T) {
	wf, _, pod, _ := diagnosticFixture()
	pod.Spec.NodeName = "node-a"
	started := pod.Status.ContainerStatuses[0].State.Terminated.StartedAt
	pod.Status.StartTime = &started
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses[1].ContainerID = "containerd://" + strings.Repeat("c", 64)
	pod.Status.ContainerStatuses[1].State.Terminated.StartedAt = started
	pod.Status.ContainerStatuses[1].State.Terminated.FinishedAt = pod.Status.ContainerStatuses[0].State.Terminated.FinishedAt
	source, recorder := &resourceSource{}, &resourceRecorder{}
	c := &Controller{kube: fake.NewClientset(pod), resourceSource: source, resourceRecorder: recorder}
	finished := map[string]time.Time{}
	if err := c.sampleResourceUsageWithState(t.Context(), wf, wf.Annotations[runIDAnnotation], finished); err != nil {
		t.Fatal(err)
	}
	if len(recorder.values) != 2 || !recorder.values[0].PodFinishedAt.IsZero() {
		t.Fatal("fixture did not precede terminal phase", recorder.values)
	}
	// Exercise the same transition with the bounded cache already full.
	for len(finished) < 4096 {
		finished[fmt.Sprint(len(finished))] = time.Time{}
	}
	pod.Status.Phase = corev1.PodFailed
	c.kube = fake.NewClientset(pod)
	if err := c.sampleResourceUsageWithState(t.Context(), wf, wf.Annotations[runIDAnnotation], finished); err != nil {
		t.Fatal(err)
	}
	if len(recorder.values) != 4 || recorder.values[2].PodFinishedAt.IsZero() || recorder.values[3].PodFinishedAt.IsZero() {
		t.Fatal("suppression lost late pod finish", recorder.values)
	}
	if err := c.sampleResourceUsageWithState(t.Context(), wf, wf.Annotations[runIDAnnotation], finished); err != nil {
		t.Fatal(err)
	}
	if len(recorder.values) != 4 || source.calls != 2 || len(finished) != 4096 {
		t.Fatal("pod finish update did not return to suppression", len(recorder.values), source.calls, len(finished))
	}
}
