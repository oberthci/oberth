package argojob

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
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

// Transport tests isolate credential delivery from the workflow supervisor.
func (controller *Controller) deliverExecutorTokens(ctx context.Context, wf *wfv1.Workflow, delivered map[types.UID]bool) error {
	var items []corev1.Pod
	if controller.kube != nil {
		pods, err := controller.kube.CoreV1().Pods(wf.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "workflows.argoproj.io/workflow=" + wf.Name})
		if err != nil {
			if ctx.Err() == nil && retryExecutorIdentityRead(err) {
				return nil
			}
			return identityFailure("pod_list", err)
		}
		items = pods.Items
	}
	return controller.deliverExecutorTokensObserved(ctx, wf, items, delivered, nil)
}

func TestExecutorIdentityDeliveryUsesSeparateBoundPodAndStdin(t *testing.T) {
	config := testConfig()
	config.applyDefaults()
	wf := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "run", Namespace: config.Namespace, UID: "workflow-id"}, Spec: wfv1.WorkflowSpec{ActiveDeadlineSeconds: ptr.To(int64(300))}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "step", Namespace: wf.Namespace, UID: "step-id", Labels: map[string]string{"workflows.argoproj.io/workflow": wf.Name}, Annotations: map[string]string{"workflows.argoproj.io/node-id": "node-not-yet-persisted"}, OwnerReferences: []metav1.OwnerReference{{UID: wf.UID, Kind: "Workflow", Controller: ptr.To(true)}}}, Spec: corev1.PodSpec{
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
		if a.(ktesting.CreateActionImpl).Name != config.ExecutorServiceAccount {
			t.Fatalf("token requested for %s", a.(ktesting.CreateActionImpl).Name)
		}
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
	// A new owner starts with an empty delivered map. Pods still waiting
	// receive their identity; Pods past the waiter must not receive it again.
	if err := c.deliverExecutorTokens(context.Background(), wf, map[types.UID]bool{}); err != nil {
		t.Fatal(err)
	}
	if deliveries != 2 || requests != 2 {
		t.Fatal("new owner did not resume waiting executor delivery")
	}
	pod.Status.InitContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}
	if _, err := kube.CoreV1().Pods(wf.Namespace).UpdateStatus(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := c.deliverExecutorTokens(context.Background(), wf, map[types.UID]bool{}); err != nil {
		t.Fatal(err)
	}
	if deliveries != 2 || requests != 2 {
		t.Fatal("new owner redelivered to a Pod past the waiter")
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
	if err := validateExecutorTokenTarget(pod, config.SourceSeedImage, false); err == nil {
		t.Fatal("pipeline could read executor identity")
	}
}

// #1564: Argo's wait container resolves outputs.parameters valueFrom.path and
// the script result in its own mount namespace, so every executor container
// must authenticate through the kubeconfig instead of the in-cluster default
// token path, and step containers must not be pointed at it.
func TestExecutorContainersReadIdentityThroughKubeconfig(t *testing.T) {
	config := testConfig()
	config.applyDefaults()
	wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{
		{Name: "container", Container: &corev1.Container{Image: "busybox"}},
		{Name: "script", Script: &wfv1.ScriptTemplate{Container: corev1.Container{Image: "busybox"}, Source: "true"}},
		{Name: "resource", Resource: &wfv1.ResourceTemplate{Action: "get", Manifest: "{}"}},
	}}}
	if err := projectExecutorTokens(wf, config); err != nil {
		t.Fatal(err)
	}
	for _, template := range wf.Spec.Templates {
		var patch corev1.PodSpec
		if err := json.Unmarshal([]byte(template.PodSpecPatch), &patch); err != nil {
			t.Fatal(err)
		}
		kubeconfig := map[string]bool{}
		for _, container := range append(append([]corev1.Container{}, patch.InitContainers...), patch.Containers...) {
			for _, env := range container.Env {
				if env.Name == "KUBECONFIG" {
					if env.Value != executorTokenDir+"/kubeconfig" {
						t.Fatalf("%s/%s KUBECONFIG = %q", template.Name, container.Name, env.Value)
					}
					kubeconfig[container.Name] = true
				}
			}
		}
		want := map[string]bool{"init": true, "wait": true}
		if template.Resource != nil {
			want = map[string]bool{"init": true, "main": true}
		}
		if !reflect.DeepEqual(kubeconfig, want) {
			t.Fatalf("%s: executor containers with KUBECONFIG = %v, want %v", template.Name, kubeconfig, want)
		}
	}
}

func TestExecutorTokenDirectoryIsFreshPerDelivery(t *testing.T) {
	seen := map[string]bool{}
	for range 64 {
		name := executorTokenDirectory()
		if !executorDirectoryName.MatchString(name) || seen[name] {
			t.Fatalf("directory %q", name)
		}
		seen[name] = true
	}
}

// #1564 review: Argo's progress monitor keeps patching WorkflowTaskResults
// while wait reads a step's outputs, re-reading the token file on a low
// descriptor. The server patch must zero both ticks on every executor container
// and win over the values the controller injects. Repositories cannot reach
// these containers' env: podSpecPatch is refused at admission and Argo exposes
// no authored env for init/wait.
func TestExecutorProgressMonitorIsDisabledOverControllerValues(t *testing.T) {
	config := testConfig()
	config.applyDefaults()
	wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{
		{Name: "container", Container: &corev1.Container{Image: "busybox"}},
		{Name: "resource", Resource: &wfv1.ResourceTemplate{Action: "get", Manifest: "{}"}},
	}}}
	if err := projectExecutorTokens(wf, config); err != nil {
		t.Fatal(err)
	}
	controllerEnv := []corev1.EnvVar{
		{Name: "ARGO_PROGRESS_FILE", Value: "/var/run/argo/progress"},
		{Name: "ARGO_PROGRESS_PATCH_TICK_DURATION", Value: "1m0s"},
		{Name: "ARGO_PROGRESS_FILE_TICK_DURATION", Value: "3s"},
	}
	for _, template := range wf.Spec.Templates {
		executors := []string{"init", "wait"}
		if template.Resource != nil {
			executors = []string{"init", "main"}
		}
		before := corev1.PodSpec{InitContainers: []corev1.Container{{Name: "init", Env: controllerEnv}}}
		for _, name := range executors[1:] {
			before.Containers = append(before.Containers, corev1.Container{Name: name, Env: controllerEnv})
		}
		after := applyCredentialWaitPatch(t, before, template.PodSpecPatch)
		for _, container := range append(append([]corev1.Container{}, after.InitContainers...), after.Containers...) {
			if container.Name == executorTokenWaiter {
				continue
			}
			values := map[string]string{}
			for _, env := range container.Env {
				if _, seen := values[env.Name]; seen {
					t.Fatalf("%s/%s duplicates %s", template.Name, container.Name, env.Name)
				}
				values[env.Name] = env.Value
			}
			for _, name := range []string{"ARGO_PROGRESS_PATCH_TICK_DURATION", "ARGO_PROGRESS_FILE_TICK_DURATION"} {
				// Argo's executor init parses these with time.ParseDuration;
				// Wait starts monitorProgress only when both are nonzero.
				if d, err := time.ParseDuration(values[name]); err != nil || d != 0 {
					t.Fatalf("%s/%s %s = %q", template.Name, container.Name, name, values[name])
				}
			}
			if values["KUBECONFIG"] != executorKubeconfig {
				t.Fatalf("%s/%s KUBECONFIG = %q", template.Name, container.Name, values["KUBECONFIG"])
			}
		}
	}
}
