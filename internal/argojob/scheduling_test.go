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
	if len(observation.Nodes) != 16 || !observation.Truncated || len(observation.Pods) != 1 || observation.Pods[0].Reason != "Unschedulable" || observation.Pods[0].Message != "Scheduler reports insufficient CPU." {
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
