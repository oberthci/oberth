package argojob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

type schedulingWorkflowClient struct {
	WorkflowClient
	workflow *wfv1.Workflow
	err      error
	gets     int
}

func (client *schedulingWorkflowClient) Get(ctx context.Context, name string, _ metav1.GetOptions) (*wfv1.Workflow, error) {
	client.gets++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if name != "wf" {
		return nil, errors.New("wrong workflow name")
	}
	return client.workflow, client.err
}
func schedulingWorkflow() *wfv1.Workflow {
	return &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "pipeline", UID: "wf-uid",
		Annotations: map[string]string{runIDAnnotation: "run"}, Labels: map[string]string{"oberth.ci/sha": "sha"}},
		Status: wfv1.WorkflowStatus{Phase: wfv1.WorkflowRunning, Nodes: wfv1.Nodes{}}}
}
func schedulingPod(name string, owner types.UID) *corev1.Pod {
	yes := true
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "pipeline", Labels: map[string]string{"workflows.argoproj.io/workflow": "wf"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: "wf", UID: owner, Controller: &yes}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending}}
}

func TestSchedulingObservationIsBoundedAndReadOnly(t *testing.T) {
	workflow := schedulingWorkflow()
	const private = "SENSITIVE-command-env-token"
	workflow.Status.Message = private
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("%02d", i)
		workflow.Status.Nodes[id] = wfv1.NodeStatus{Name: private, Phase: wfv1.NodePending, Message: "Waiting for " + private + " lock. Lock status: 0/1"}
	}
	pod := schedulingPod("own", workflow.UID)
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "Insufficient cpu " + private}}
	foreign := schedulingPod("foreign", "wrong-owner")
	foreign.Status.Phase = corev1.PodFailed
	kube := fake.NewClientset(pod, foreign)
	kube.PrependReactor("list", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		options := action.(interface{ GetListOptions() metav1.ListOptions }).GetListOptions()
		if options.Limit != maxSchedulingItems || options.LabelSelector != "workflows.argoproj.io/workflow=wf" || action.GetNamespace() != "pipeline" {
			t.Errorf("unbounded/unscoped list: %#v", options)
		}
		return false, nil, nil
	})
	client := &schedulingWorkflowClient{workflow: workflow}
	controller := &Controller{workflows: client, kube: kube, config: Config{Namespace: "pipeline"}}
	observation, err := controller.ObserveScheduling(t.Context(), "wf", "run", "sha")
	if err != nil {
		t.Fatal(err)
	}
	if len(observation.Nodes) != 16 || !observation.Truncated || len(observation.Pods) != 1 || observation.Message != observation.Pods[0].Message || observation.Pods[0].Reason != "Unschedulable" || !strings.HasPrefix(observation.Pods[0].Message, "pod pending: insufficient cpu") {
		t.Fatalf("observation = %#v", observation)
	}
	body, _ := json.Marshal(observation)
	if strings.Contains(string(body), private) || len(body) > 8192 {
		t.Fatalf("unsafe or unbounded output: %s", body)
	}
	for _, action := range kube.Actions() {
		if action.GetVerb() != "list" || action.GetResource().Resource != "pods" {
			t.Fatalf("unexpected API action: %#v", action)
		}
	}
	if client.gets != 1 {
		t.Fatalf("GET calls = %d", client.gets)
	}
}

func TestSchedulingObservationIdentityAndUnavailable(t *testing.T) {
	for _, test := range []struct {
		name    string
		change  func(*wfv1.Workflow)
		err     error
		wantErr bool
		state   string
	}{
		{name: "absent", err: apierrors.NewNotFound(schema.GroupResource{Group: "argoproj.io", Resource: "workflows"}, "wf"), state: "not_observed"},
		{name: "unavailable", err: errors.New("private transport detail"), wantErr: true},
		{name: "wrong run", change: func(w *wfv1.Workflow) { w.Annotations[runIDAnnotation] = "other" }, wantErr: true},
		{name: "wrong SHA", change: func(w *wfv1.Workflow) { w.Labels["oberth.ci/sha"] = "other" }, wantErr: true},
		{name: "wrong namespace", change: func(w *wfv1.Workflow) { w.Namespace = "other" }, wantErr: true},
		{name: "nil pod observer", state: "observed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			workflow := schedulingWorkflow()
			if test.change != nil {
				test.change(workflow)
			}
			controller := &Controller{workflows: &schedulingWorkflowClient{workflow: workflow, err: test.err}, config: Config{Namespace: "pipeline"}}
			observation, err := controller.ObserveScheduling(t.Context(), "wf", "run", "sha")
			if (err != nil) != test.wantErr {
				t.Fatalf("err=%v", err)
			}
			if !test.wantErr && observation.State != test.state {
				t.Fatalf("observation=%#v", observation)
			}
		})
	}
}

func TestSchedulingObservationPodListErrorAndCancellation(t *testing.T) {
	kube := fake.NewClientset()
	kube.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("private transport error")
	})
	controller := &Controller{workflows: &schedulingWorkflowClient{workflow: schedulingWorkflow()}, kube: kube, config: Config{Namespace: "pipeline"}}
	observation, err := controller.ObserveScheduling(t.Context(), "wf", "run", "sha")
	if err != nil || len(observation.Pods) != 1 || observation.Pods[0].Reason != "Unavailable" {
		t.Fatalf("observation=%#v err=%v", observation, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := controller.ObserveScheduling(ctx, "wf", "run", "sha"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestSchedulingPodReasons(t *testing.T) {
	for _, test := range []struct {
		name          string
		phase         corev1.PodPhase
		waiting, want string
	}{
		{"pending", corev1.PodPending, "", "Pending"},
		{"startup", corev1.PodPending, "ContainerCreating", "ContainerCreating"},
		{"image", corev1.PodPending, "ImagePullBackOff", "ImagePullBackOff"},
		{"untrusted", corev1.PodPending, "SECRET command", "Waiting"},
		{"running", corev1.PodRunning, "", ""},
		{"terminal", corev1.PodSucceeded, "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			pod := schedulingPod("pod", "wf-uid")
			pod.Status.Phase = test.phase
			if test.waiting != "" {
				pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "SECRET", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: test.waiting, Message: "SECRET command"}}}}
			}
			result := podSchedulingReason(*pod)
			if result.Reason != test.want {
				t.Fatalf("result=%#v", result)
			}
			body, _ := json.Marshal(result)
			if strings.Contains(string(body), "SECRET") {
				t.Fatalf("leak: %s", body)
			}
		})
	}
}

func TestSchedulingObservationCapacityReasons(t *testing.T) {
	for _, tc := range []struct {
		message, want string
	}{
		{"0/3 nodes are available: 3 Insufficient ephemeral-storage.", "pod pending: insufficient ephemeral-storage; pod requests 3Gi ephemeral storage."},
		{"0/1 nodes are available: 1 Too many pods.", "pod pending: too many pods; waiting for a pod slot"},
		{"Insufficient ephemeral-storage-policy", "pod pending: not scheduled"},
		{"Too many pods-required", "pod pending: not scheduled"},
	} {
		t.Run(tc.message, func(t *testing.T) {
			const private = "SENSITIVE-command-env-token"
			workflow := schedulingWorkflow()
			pod := schedulingPod("waiting", workflow.UID)
			pod.Spec.Containers = []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("3Gi")}}}}
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: tc.message + " " + private}}
			controller := &Controller{workflows: &schedulingWorkflowClient{workflow: workflow}, kube: fake.NewClientset(pod), config: Config{Namespace: "pipeline"}}
			observation, err := controller.ObserveScheduling(t.Context(), "wf", "run", "sha")
			if err != nil {
				t.Fatal(err)
			}
			if len(observation.Pods) != 1 || observation.Pods[0].Message != tc.want {
				t.Fatalf("pod capacity reason = %+v, want %q", observation.Pods, tc.want)
			}
			if (strings.HasPrefix(tc.want, "pod pending: insufficient") || strings.HasPrefix(tc.want, "pod pending: too many")) && observation.Message != tc.want {
				t.Fatalf("run reason = %q, want %q", observation.Message, tc.want)
			}
			body, err := json.Marshal(observation)
			if err != nil || strings.Contains(string(body), private) {
				t.Fatalf("unsafe observation: %s, %v", body, err)
			}
		})
	}
}

// An unschedulable pod reports what it asks for, so an impossible job on a
// small node is diagnosable from the run status (#024).
func TestUnschedulablePodReportsItsRequest(t *testing.T) {
	t.Parallel()
	pod := corev1.Pod{
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}}}},
			Containers: []corev1.Container{
				{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("6Gi")}}},
				{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")}}},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable",
			Message: "0/1 nodes are available: 1 Insufficient memory.",
		}}},
	}
	got := podSchedulingReason(pod)
	if got.Reason != "Unschedulable" || !strings.Contains(got.Message, "8Gi requested") {
		t.Fatalf("reason = %+v", got)
	}
}

func TestAllPendingPodMessagesUseUniformPrefix(t *testing.T) {
	for _, kind := range []string{"unscheduled", "unknown capacity", "cpu", "memory", "ephemeral", "pod slots", "volume", "container", "generic", "deadline"} {
		t.Run(kind, func(t *testing.T) {
			pod := corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}}
			condition := corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable"}
			switch kind {
			case "unscheduled":
				condition.Reason = "Other"
				pod.Status.Conditions = []corev1.PodCondition{condition}
			case "unknown capacity":
				condition.Message = "SENSITIVE unknown scheduler detail"
				pod.Status.Conditions = []corev1.PodCondition{condition}
			case "cpu", "memory", "ephemeral", "pod slots", "volume":
				condition.Message = map[string]string{"cpu": "Insufficient cpu", "memory": "Insufficient memory", "ephemeral": "Insufficient ephemeral-storage", "pod slots": "Too many pods", "volume": "unbound immediate PersistentVolumeClaims"}[kind] + " SENSITIVE suffix"
				pod.Status.Conditions = []corev1.PodCondition{condition}
			case "container":
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "SENSITIVE container message"}}}}
			case "deadline":
				pod.Status.Reason = "DeadlineExceeded"
			}
			got := podSchedulingReason(pod)
			if !strings.HasPrefix(got.Message, "pod pending: ") || strings.Contains(got.Message, "SENSITIVE") {
				t.Fatalf("unsafe/inconsistent Pending reason: %+v", got)
			}
		})
	}
}

func TestPendingCPUMessageUsesPodRequests(t *testing.T) {
	pod := corev1.Pod{
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3500m")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("16")}}},
			{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")}}},
		}},
		Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "0/3 nodes are available: 3 Insufficient cpu."}}},
	}
	got := podSchedulingReason(pod)
	if got.Message != "pod pending: insufficient cpu (4 CPU requested)" {
		t.Fatal(got.Message)
	}
}

// The reason line says which lock a run is queued behind, in the controller's
// own lock-key grammar; anything outside it falls back to the fixed form.
func TestLockWaitMessageNamesTheLockWithoutFreeText(t *testing.T) {
	for message, want := range map[string]string{
		"Waiting for pipeline/ConfigMap/oberth-sync-limits/kvm lock. Lock status: 0/1": "Waiting for lock ConfigMap/oberth-sync-limits/kvm (0/1 free).",
		"Waiting for pipeline/Mutex/oberth-release lock. Lock status: 0/1 ":            "Waiting for lock Mutex/oberth-release (0/1 free).",
		"Waiting for pipeline/ConfigMap/limits/heavy-go lock. Lock status: 1/2":        "Waiting for lock ConfigMap/limits/heavy-go (1/2 free).",
		"Waiting for SENSITIVE-command-env-token lock. Lock status: 0/1":               "Waiting for lock (synchronization).",
		"Waiting for $(curl evil) lock. Lock status: 0/1":                              "Waiting for lock (synchronization).",
		"Waiting for pipeline/Secret/token lock. Lock status: 0/1":                     "Waiting for lock (synchronization).",
		"Waiting for pipeline/Mutex/name;rm -rf lock. Lock status: 0/1":                "Waiting for lock (synchronization).",
	} {
		if got := safeWorkflowMessage(message); got != want {
			t.Fatalf("safeWorkflowMessage(%q) = %q, want %q", message, got, want)
		}
	}
}
