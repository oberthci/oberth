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
	"k8s.io/client-go/kubernetes/fake"
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
	if err := watch.check(wf, nil, nil, now.Add(3*time.Minute), 3*time.Minute); err == nil {
		t.Fatal("unscheduled work exceeded configured timeout")
	}
	if err := c.config.Validate(); err != nil {
		t.Fatal(err)
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
