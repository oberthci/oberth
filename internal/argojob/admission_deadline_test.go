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
)

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

func TestAdmissionDeadlineRetrySurvivesObserverRestartAndIgnoresForeignPod(t *testing.T) {
	_, wf, pod, _ := executorRetryFixture(t)
	now := time.Now()
	wf.CreationTimestamp = metav1.NewTime(now)
	pod.Spec.NodeName = "worker"
	pod.Annotations = map[string]string{executorIdentityRetryAnnotation: now.Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)}
	if err := new(executorIdentityProgress).check(wf, []corev1.Pod{*pod}, nil, now); err == nil || !strings.Contains(err.Error(), "after one issuance retry") {
		t.Fatal("restart reset identity issuance budget", err)
	}
	pod.OwnerReferences[0].UID = "foreign"
	wf.CreationTimestamp = metav1.NewTime(now.Add(-10 * time.Minute))
	if err := new(executorIdentityProgress).check(wf, []corev1.Pod{*pod}, nil, now); err != nil {
		t.Fatal("foreign pod kept admitted workflow alive", err)
	}
}
