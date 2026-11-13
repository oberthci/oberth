package argojob

import (
	"context"
	"io"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestCapabilityResourceExecutorUsesLiveOperatorAuthority(t *testing.T) {
	c, wf, pod, requests := executorRetryFixture(t)
	grant := operatorCapability()
	wf.Annotations[capabilityAnnotation] = capabilityBinding(7, grant)
	wf.Spec.ServiceAccountName = c.config.PipelineServiceAccount
	wf.Spec.AutomountServiceAccountToken = ptr.To(false)
	wf.Spec.Executor = &wfv1.ExecutorConfig{ServiceAccountName: c.config.ExecutorServiceAccount}
	wf.Spec.Templates = []wfv1.Template{{Name: "task", ServiceAccountName: c.config.PipelineServiceAccount, AutomountServiceAccountToken: ptr.To(false), Resource: &wfv1.ResourceTemplate{Action: "get"}, Executor: &wfv1.ExecutorConfig{ServiceAccountName: grant.ServiceAccount}}}
	wf.Status.Nodes = map[string]wfv1.NodeStatus{"task-node": {TemplateName: "task"}}
	pod.Annotations = map[string]string{"workflows.argoproj.io/node-id": "task-node"}
	pod.Spec.ServiceAccountName = c.config.PipelineServiceAccount
	pod.Spec.Containers[0].Image = argoworkflow.ExecutorImage
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: executorTokenVolume, MountPath: executorTokenDir}}
	if _, err := c.kube.CoreV1().Pods(wf.Namespace).Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	kube := c.kube.(*fake.Clientset)
	kube.Resources = operatorKubeClient(grant).Resources
	if err := kube.Tracker().Add(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: grant.ServiceAccount, Namespace: wf.Namespace}}); err != nil {
		t.Fatal(err)
	}
	kube.PrependReactor("create", "serviceaccounts", func(action ktesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "token" && action.(ktesting.CreateActionImpl).Name != "operator-task-runner" {
			t.Fatal("resource executor received shared executor authority")
		}
		return false, nil, nil
	})
	bindCapabilityLoader(t, c, func(_ context.Context, repoID int64) (*argoworkflow.Capability, error) {
		if repoID != 7 {
			t.Fatal("executor grant read escaped repository")
		}
		return grant, nil
	})
	deliveries := 0
	c.seeder.exec = func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
		deliveries++
		return nil
	}
	if err := c.deliverExecutorTokens(t.Context(), wf, map[types.UID]bool{}); err != nil || deliveries != 1 || *requests != 1 {
		t.Fatalf("operator executor delivery: requests=%d deliveries=%d err=%v", *requests, deliveries, err)
	}
	grant = nil
	if err := c.deliverExecutorTokens(t.Context(), wf, map[types.UID]bool{}); err == nil || deliveries != 1 || *requests != 1 {
		t.Fatal("revoked operator authority issued another token")
	}
	if err := c.retireExecutorIdentity(t.Context(), wf); err != nil {
		t.Fatal(err)
	}
	if _, err := c.kube.CoreV1().Pods(wf.Namespace).Get(t.Context(), c.identityPodName(wf.UID, "operator-task-runner"), metav1.GetOptions{}); err == nil {
		t.Fatal("operator identity lifetime pod survived cleanup after revocation")
	}
}
