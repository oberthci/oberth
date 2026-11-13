package argojob

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSchedulingPlacementReasonsReadOnly(t *testing.T) {
	for _, tc := range []struct{ message, want string }{
		{"0/1 nodes are available: 1 Insufficient example.com/widget.", "Insufficient example.com/widget"},
		{"0/2 nodes are available: 2 node(s) didn't match pod anti-affinity rules.", "node(s) didn't match pod anti-affinity rules"},
		{"0/2 nodes are available: 2 node(s) didn't satisfy existing pods anti-affinity rules.", "node(s) didn't satisfy existing pods anti-affinity rules"},
		{"Insufficient example.com/widget, Insufficient vendor.example/device.", "Insufficient example.com/widget; Insufficient vendor.example/device"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			workflow := schedulingWorkflow()
			pod := schedulingPod("own", workflow.UID)
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: tc.message + " PRIVATE suffix"}}
			foreign := schedulingPod("foreign", "other-owner")
			foreign.Status.Conditions = pod.Status.Conditions
			kube := fake.NewClientset(pod, foreign)
			controller := &Controller{workflows: &schedulingWorkflowClient{workflow: workflow}, kube: kube, config: Config{Namespace: "pipeline"}}
			got, err := controller.ObserveScheduling(t.Context(), "wf", "run", "sha")
			if err != nil {
				t.Fatal(err)
			}
			if got.Message != "pod pending: "+tc.want || len(got.Pods) != 1 || got.Pods[0].Message != got.Message {
				t.Fatalf("observation = %+v", got)
			}
			body, _ := json.Marshal(got)
			if strings.Contains(string(body), "PRIVATE") {
				t.Fatalf("unrelated text exposed: %s", body)
			}
			for _, a := range kube.Actions() {
				if a.GetVerb() != "list" || a.GetResource().Resource != "pods" {
					t.Fatalf("unexpected action: %+v", a)
				}
			}
		})
	}
	pod := schedulingPod("pod", "uid")
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Other", Message: "Insufficient example.com/widget"}}
	if got := schedulerPlacementReason(*pod); got != "" {
		t.Fatalf("non-scheduler reason = %q", got)
	}
	pod.Status.Conditions[0].Reason = "Unschedulable"
	pod.Status.Phase = corev1.PodRunning
	if got := schedulerPlacementReason(*pod); got != "" {
		t.Fatalf("running Pod reason = %q", got)
	}
}
