package argojob

import (
	"errors"
	"io"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

func TestExecutorIdentityRetiresCutoverHolders(t *testing.T) {
	c, wf, _, _ := executorRetryFixture(t)
	kube := c.kube.(*fake.Clientset)
	for _, name := range []string{"oberth-executor-oldhash", "oberth-executor-oldcapability", c.identityPodName(wf, c.config.ExecutorServiceAccount), "foreign-run", "no-role", "not-workflow", "not-controller", "gated-step", "identity-plus-main", "ungated-identity"} {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: wf.Namespace, UID: types.UID(name), Labels: map[string]string{"oberth.ci/role": "executor-identity"}, OwnerReferences: []metav1.OwnerReference{{UID: wf.UID, Kind: "Workflow", Controller: ptr.To(true)}}}, Spec: holderSpec("")}
		switch name {
		case "gated-step":
			// A step Pod carries this owner shape; a granted pod patcher could add the label.
			pod.Spec.Containers = []corev1.Container{{Name: "wait"}, {Name: "main"}}
		case "identity-plus-main":
			pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "main"})
		case "ungated-identity":
			pod.Spec.SchedulingGates = nil
		case "foreign-run":
			pod.OwnerReferences[0].UID = "other-uid"
		case "no-role":
			pod.Labels = nil
		case "not-workflow":
			pod.OwnerReferences[0].Kind = "Pod"
		case "not-controller":
			pod.OwnerReferences[0].Controller = nil
		}
		if err := kube.Tracker().Add(pod); err != nil {
			t.Fatal(err)
		}
	}
	deletes := 0
	kube.PrependReactor("delete", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
		action := a.(ktesting.DeleteAction)
		if action.GetDeleteOptions().Preconditions == nil || action.GetDeleteOptions().Preconditions.UID == nil || string(*action.GetDeleteOptions().Preconditions.UID) != action.GetName() {
			t.Fatal("holder deletion lost UID precondition")
		}
		deletes++
		return false, nil, nil
	})
	for range 2 {
		if err := c.retireExecutorIdentity(t.Context(), wf); err != nil {
			t.Fatal(err)
		}
	}
	if deletes != 3 {
		t.Fatalf("retired %d holders, want old executor, old capability and current holder", deletes)
	}
}

func TestExecutorIdentityRetryRetiresOnlyItsAccount(t *testing.T) {
	c, wf, _, _ := executorRetryFixture(t)
	for _, account := range []string{"executor", "capability"} {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "oberth-executor-old-" + account, Namespace: wf.Namespace, UID: types.UID(account), Labels: map[string]string{"oberth.ci/role": "executor-identity"}, OwnerReferences: []metav1.OwnerReference{{UID: wf.UID, Kind: "Workflow", Controller: ptr.To(true)}}}, Spec: holderSpec(account)}
		if _, err := c.kube.CoreV1().Pods(wf.Namespace).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.retireIdentityPod(t.Context(), wf, "executor"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.kube.CoreV1().Pods(wf.Namespace).Get(t.Context(), "oberth-executor-old-executor", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("old account holder survived: %v", err)
	}
	if _, err := c.kube.CoreV1().Pods(wf.Namespace).Get(t.Context(), "oberth-executor-old-capability", metav1.GetOptions{}); err != nil {
		t.Fatal("unrelated capability holder retired", err)
	}
}

// holderSpec is the shape every holder since v0.22.4 is created with: one
// scheduling gate and the single identity container.
func holderSpec(account string) corev1.PodSpec {
	return corev1.PodSpec{ServiceAccountName: account, SchedulingGates: []corev1.PodSchedulingGate{{Name: "oberth.ci/executor-identity"}}, Containers: []corev1.Container{{Name: "identity"}}}
}

func TestExecutorIdentityRetirementListFailureFailsSettleClosed(t *testing.T) {
	c, wf, binding, events := restartCutFixture(t, func(_ *wfv1.Workflow, _ *wfv1.NodeStatus, _ *corev1.Pod) {})
	withTestObservationCache(t, c)
	c.kube.(*fake.Clientset).PrependReactor("list", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		if action.(ktesting.ListAction).GetListRestrictions().Labels.String() == "oberth.ci/role=executor-identity" {
			return true, nil, errors.New("API unavailable")
		}
		return false, nil, nil
	})
	_, err := c.WaitResumed(t.Context(), wf.Name, wf.Annotations[runIDAnnotation], io.Discard, events, 0, binding)
	if err == nil || !strings.Contains(err.Error(), "retire executor identity: API unavailable") {
		t.Fatalf("settle ignored a failed holder retirement: %v", err)
	}
}
