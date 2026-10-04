package argojob

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

func TestExecutorIdentityDeliveryUsesSeparateBoundPodAndStdin(t *testing.T) {
	config := testConfig()
	config.applyDefaults()
	wf := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "run", Namespace: config.Namespace, UID: "workflow-id"}, Spec: wfv1.WorkflowSpec{ActiveDeadlineSeconds: ptr.To(int64(300))}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "step", Namespace: wf.Namespace, UID: "step-id", Labels: map[string]string{"workflows.argoproj.io/workflow": wf.Name}, OwnerReferences: []metav1.OwnerReference{{UID: wf.UID, Kind: "Workflow", Controller: ptr.To(true)}}}, Spec: corev1.PodSpec{
		ServiceAccountName: config.PipelineServiceAccount,
		Volumes:            []corev1.Volume{{Name: executorTokenVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}}},
		InitContainers:     []corev1.Container{{Name: executorTokenWaiter, Image: config.SourceSeedImage, Command: []string{"sh", "-c", executorTokenWaitCommand}, VolumeMounts: []corev1.VolumeMount{{Name: executorTokenVolume, MountPath: executorTokenDir}}}},
		Containers:         []corev1.Container{{Name: "main"}},
	}, Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{Name: executorTokenWaiter, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	kube := fake.NewClientset(pod, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: wf.Namespace}, Data: map[string]string{"ca.crt": "public-ca"}})
	kube.PrependReactor("create", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
		p := a.(ktesting.CreateAction).GetObject().(*corev1.Pod)
		p.UID = "identity-id"
		return false, nil, nil
	})
	requests := 0
	kube.PrependReactor("create", "serviceaccounts", func(a ktesting.Action) (bool, runtime.Object, error) {
		requests++
		req := a.(ktesting.CreateAction).GetObject().(*authenticationv1.TokenRequest)
		if a.GetSubresource() != "token" || req.Spec.BoundObjectRef.UID != "identity-id" || req.Spec.BoundObjectRef.Name == pod.Name {
			t.Fatal("executor token bound to pipeline identity")
		}
		return true, &authenticationv1.TokenRequest{Status: authenticationv1.TokenRequestStatus{Token: "test-executor-token", ExpirationTimestamp: metav1.NewTime(time.Now().Add(time.Hour))}}, nil
	})
	deliveries := 0
	c := &Controller{kube: kube, config: config, seeder: &SourceSeeder{exec: func(_ context.Context, ns, name, container string, command []string, input io.Reader, _, _ io.Writer) error {
		deliveries++
		if ns != wf.Namespace || name != pod.Name || container != executorTokenWaiter {
			t.Fatal("delivery escaped owned waiter")
		}
		body, _ := io.ReadAll(input)
		if !strings.HasPrefix(string(body), "test-executor-token\n") {
			t.Fatal("token not on stdin")
		}
		if strings.Contains(strings.Join(command, " "), "test-executor-token") {
			t.Fatal("credential in exec arguments")
		}
		return nil
	}}}
	delivered := map[types.UID]bool{}
	for range 2 {
		if err := c.deliverExecutorTokens(context.Background(), wf, delivered); err != nil {
			t.Fatal(err)
		}
	}
	if deliveries != 1 || requests != 1 {
		t.Fatal("completed delivery repeated")
	}
	for _, a := range kube.Actions() {
		if a.GetResource().Resource == "secrets" {
			t.Fatal("executor delivery accessed Kubernetes Secrets")
		}
	}
	if err := c.retireExecutorIdentity(context.Background(), wf); err != nil {
		t.Fatal(err)
	}
	if err := c.retireExecutorIdentity(context.Background(), wf); err != nil {
		t.Fatal(err)
	}
	holders, err := kube.CoreV1().Pods(wf.Namespace).List(context.Background(), metav1.ListOptions{LabelSelector: "oberth.ci/role=executor-identity"})
	if err != nil || len(holders.Items) != 0 {
		t.Fatal("completed workflow retained executor identity")
	}
	// The main container must never gain access even if the rest of the Pod is valid.
	pod.Spec.Containers[0].VolumeMounts = pod.Spec.InitContainers[0].VolumeMounts
	if err := validateExecutorTokenTarget(pod, config.SourceSeedImage); err == nil {
		t.Fatal("pipeline could read executor identity")
	}
}
