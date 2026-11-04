package argojob

import (
	"context"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	wffake "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned/fake"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

func TestAdmissionDeadlineWorkflowWithOnlyGatedIdentityPod(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		config := testConfig()
		wf := schedulingWorkflow()
		wf.Namespace = config.Namespace
		wf.CreationTimestamp = metav1.NewTime(time.Now())
		wf.Status.Phase = wfv1.WorkflowRunning
		wf.Status.Nodes = nil
		wf.Spec.ActiveDeadlineSeconds = ptr.To(int64(7200))
		holder := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "oberth-executor-holder", Namespace: wf.Namespace, UID: "holder", Labels: map[string]string{"workflows.argoproj.io/workflow": wf.Name, "oberth.ci/role": "executor-identity"}, OwnerReferences: []metav1.OwnerReference{{UID: wf.UID, Kind: "Workflow", Controller: ptr.To(true)}}}, Spec: corev1.PodSpec{SchedulingGates: []corev1.PodSchedulingGate{{Name: "oberth.ci/executor-identity"}}}}
		client := wffake.NewSimpleClientset(wf)
		c, err := NewController(client.ArgoprojV1alpha1().Workflows(config.Namespace), fake.NewClientset(holder), config)
		if err != nil {
			t.Fatal(err)
		}
		c.interval = time.Minute
		ctx, cancel := context.WithTimeout(t.Context(), 11*time.Minute)
		defer cancel()
		_, err = c.Wait(ctx, wf.Name, wf.Annotations[runIDAnnotation], io.Discard)
		if err == nil || !strings.Contains(err.Error(), "no scheduled pod or progress for 10m") {
			t.Fatalf("stuck granted workflow did not fail clearly: %v", err)
		}
	})
}

func TestAdmissionDeadlineIdentityRetriesIssuanceOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, wf, pod, requests := executorRetryFixture(t)
		wf.CreationTimestamp = metav1.NewTime(time.Now())
		wf.Status.Phase = wfv1.WorkflowRunning
		pod.Spec.NodeName = "worker"
		if _, err := c.kube.CoreV1().Pods(wf.Namespace).Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		c.workflows = wffake.NewSimpleClientset(wf).ArgoprojV1alpha1().Workflows(wf.Namespace)
		c.interval = time.Minute
		c.seeder.exec = func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
			return nil
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		defer cancel()
		start := time.Now()
		_, err := c.Wait(ctx, wf.Name, "identity-run", io.Discard)
		if err == nil || !strings.Contains(err.Error(), "executor identity readiness deadline exceeded after one issuance retry") || *requests != 2 || time.Since(start) != 4*time.Minute {
			t.Fatalf("identity deadline/retry: requests=%d elapsed=%s err=%v", *requests, time.Since(start), err)
		}
	})
}

func TestAdmissionDeadlineIgnoresHealthyScheduledWorkAndHonorsConfiguration(t *testing.T) {
	c, wf, pod, _ := executorRetryFixture(t)
	now := time.Now()
	wf.CreationTimestamp = metav1.NewTime(now.Add(-time.Hour))
	pod.Spec.NodeName = "worker"
	pod.Status.InitContainerStatuses = nil
	watch := admissionProgress{}
	if err := watch.check(wf, []corev1.Pod{*pod}, nil, now, 3*time.Minute); err != nil {
		t.Fatal("scheduled work expired", err)
	}
	if err := watch.check(wf, nil, nil, now.Add(2*time.Minute), 3*time.Minute); err != nil {
		t.Fatal("configured timeout ignored", err)
	}
	if err := watch.check(wf, nil, nil, now.Add(time.Hour), 3*time.Minute); err != nil {
		t.Fatal("workflow startup deadline restarted between steps", err)
	}
	unstarted := admissionProgress{}
	wf.CreationTimestamp = metav1.NewTime(now)
	if err := unstarted.check(wf, nil, nil, now.Add(3*time.Minute), 3*time.Minute); err == nil {
		t.Fatal("unstarted workflow exceeded configured timeout")
	}
	if err := c.config.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestAdmissionDeadlineLeavesCapacityWaitToKubernetes(t *testing.T) {
	for _, message := range []string{"0/3 nodes are available: 3 Insufficient cpu.", "Insufficient memory", "Insufficient example.com/gpu"} {
		t.Run(message, func(t *testing.T) {
			_, wf, pod, _ := executorRetryFixture(t)
			now := time.Now()
			wf.CreationTimestamp = metav1.NewTime(now.Add(-time.Hour))
			pod.Spec.NodeName = ""
			pod.Status = corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: message}}}
			watch := admissionProgress{}
			for _, at := range []time.Time{now, now.Add(2 * time.Hour)} {
				if err := watch.check(wf, []corev1.Pod{*pod}, nil, at, 10*time.Minute); err != nil {
					t.Fatal("Kubernetes capacity wait failed workflow", err)
				}
			}
			pod.Status.Conditions[0].Message = "node(s) had untolerated taint"
			if err := watch.check(wf, []corev1.Pod{*pod}, nil, now.Add(2*time.Hour+10*time.Minute), 10*time.Minute); err == nil {
				t.Fatal("non-capacity startup stall escaped deadline")
			}
		})
	}
}

func TestAdmissionDeadlineCapacityPendingWorkflowCanCompleteAfterStartupBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		config := testConfig()
		wf := schedulingWorkflow()
		wf.Namespace = config.Namespace
		wf.CreationTimestamp = metav1.NewTime(time.Now())
		wf.Status.Phase, wf.Status.Nodes = wfv1.WorkflowRunning, nil
		wf.Spec.ActiveDeadlineSeconds = ptr.To(int64(7200))
		begin := time.Now()
		client := wffake.NewSimpleClientset(wf)
		client.PrependReactor("get", "workflows", func(ktesting.Action) (bool, runtime.Object, error) {
			observed := wf.DeepCopy()
			if time.Since(begin) >= 12*time.Minute {
				observed.Status.Phase = wfv1.WorkflowSucceeded
			}
			return true, observed, nil
		})
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "waiting", Namespace: wf.Namespace, UID: "waiting", Labels: map[string]string{"workflows.argoproj.io/workflow": wf.Name}, OwnerReferences: []metav1.OwnerReference{{UID: wf.UID, Kind: "Workflow", Controller: ptr.To(true)}}}, Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "0/3 nodes are available: 3 Insufficient cpu."}}}}
		controller, err := NewController(client.ArgoprojV1alpha1().Workflows(config.Namespace), fake.NewClientset(pod), config)
		if err != nil {
			t.Fatal(err)
		}
		controller.interval = time.Minute
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
		defer cancel()
		result, err := controller.Wait(ctx, wf.Name, wf.Annotations[runIDAnnotation], io.Discard)
		if err != nil || !result.Succeeded || time.Since(begin) != 12*time.Minute {
			t.Fatalf("capacity wait was not left to Kubernetes: elapsed=%s result=%+v err=%v", time.Since(begin), result, err)
		}
	})
}

func TestAdmissionDeadlineCapacityExemptionRequiresOwnedWorkloadPod(t *testing.T) {
	for _, mode := range []string{"foreign", "holder", "gated", "wrong-condition"} {
		t.Run(mode, func(t *testing.T) {
			_, wf, pod, _ := executorRetryFixture(t)
			now := time.Now()
			wf.CreationTimestamp = metav1.NewTime(now.Add(-time.Hour))
			pod.Spec.NodeName = ""
			pod.Status = corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "Insufficient cpu"}}}
			switch mode {
			case "foreign":
				pod.OwnerReferences[0].UID = "foreign"
			case "holder":
				pod.Labels["oberth.ci/role"] = "executor-identity"
			case "gated":
				pod.Status.Conditions[0].Reason = "SchedulingGated"
			case "wrong-condition":
				pod.Status.Conditions[0].Type = corev1.PodReady
			}
			if err := new(admissionProgress).check(wf, []corev1.Pod{*pod}, nil, now, 10*time.Minute); err == nil {
				t.Fatal("unrelated pod/condition kept unstarted workflow alive")
			}
		})
	}
}

func TestAdmissionDeadlineRemembersScheduledPodAfterObserverRestart(t *testing.T) {
	_, wf, _, _ := executorRetryFixture(t)
	now := time.Now()
	wf.CreationTimestamp = metav1.NewTime(now.Add(-time.Hour))
	wf.Status.Nodes = wfv1.Nodes{"finished": {Type: wfv1.NodeTypePod, Phase: wfv1.NodeSucceeded, HostNodeName: "worker", FinishedAt: metav1.NewTime(now.Add(-time.Hour))}}
	if err := new(admissionProgress).check(wf, nil, nil, now, 10*time.Minute); err != nil {
		t.Fatal("workflow startup deadline restarted after completed step", err)
	}
}

func TestAdmissionDeadlineMissingWaiterDoesNotChargePriorCapacityWait(t *testing.T) {
	_, wf, pod, _ := executorRetryFixture(t)
	now := time.Now()
	wf.CreationTimestamp = metav1.NewTime(now.Add(-time.Hour))
	pod.CreationTimestamp = wf.CreationTimestamp
	pod.Status = corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "Insufficient cpu"}}}
	if err := new(admissionProgress).check(wf, []corev1.Pod{*pod}, nil, now, 10*time.Minute); err != nil {
		t.Fatal("capacity wait charged against identity readiness", err)
	}
	pod.Spec.NodeName = "worker"
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(now)}}
	watch := admissionProgress{} // reconstruct from durable scheduling evidence
	if err := watch.check(wf, []corev1.Pod{*pod}, nil, now.Add(9*time.Minute), 10*time.Minute); err != nil {
		t.Fatal("pre-scheduling wait consumed identity observation budget", err)
	}
	if err := watch.check(wf, []corev1.Pod{*pod}, nil, now.Add(10*time.Minute), 10*time.Minute); err == nil || !strings.Contains(err.Error(), "readiness not observed") {
		t.Fatal("missed waiter event escaped its post-scheduling bound", err)
	}
}

func TestAdmissionDeadlineRetrySurvivesObserverRestartAndIgnoresForeignPod(t *testing.T) {
	_, wf, pod, _ := executorRetryFixture(t)
	now := time.Now()
	wf.CreationTimestamp = metav1.NewTime(now)
	pod.Spec.NodeName = "worker"
	pod.Annotations = map[string]string{executorIdentityRetryAnnotation: now.Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)}
	if err := new(admissionProgress).check(wf, []corev1.Pod{*pod}, nil, now, time.Minute); err == nil || !strings.Contains(err.Error(), "after one issuance retry") {
		t.Fatal("restart reset identity issuance budget", err)
	}
	pod.OwnerReferences[0].UID = "foreign"
	wf.CreationTimestamp = metav1.NewTime(now.Add(-10 * time.Minute))
	if err := new(admissionProgress).check(wf, []corev1.Pod{*pod}, nil, now, 10*time.Minute); err == nil || !strings.Contains(err.Error(), "no scheduled pod") {
		t.Fatal("foreign pod kept admitted workflow alive", err)
	}
}
