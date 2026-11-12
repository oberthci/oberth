package argojob

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	wffake "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestNativeMissingDeadlineRemainsMissing(t *testing.T) {
	source := strings.Replace(greedyDocument, "  activeDeadlineSeconds: 999999\n", "", 1)
	wf, err := Build(testConfig(), testRequest(pipeline.TriggerCI, source))
	if err != nil {
		t.Fatal(err)
	}
	if wf.Spec.ActiveDeadlineSeconds != nil {
		t.Fatalf("injected deadline: %v", wf.Spec.ActiveDeadlineSeconds)
	}
}
func TestNativeWaitingWorkflowHasNoServerWatchdog(t *testing.T) {
	for _, declared := range []*int64{nil, ptr.To(int64(1))} {
		wf := schedulingWorkflow()
		cfg := testConfig()
		wf.Namespace = cfg.Namespace
		wf.CreationTimestamp = metav1.NewTime(time.Now().Add(-48 * time.Hour))
		wf.Status.StartedAt = metav1.Time{}
		wf.Status.Phase = wfv1.WorkflowPending
		wf.Spec.ActiveDeadlineSeconds = declared
		workflows := wffake.NewSimpleClientset(wf).ArgoprojV1alpha1().Workflows(cfg.Namespace)
		controller, err := NewController(workflows, fake.NewClientset(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		_, err = controller.Wait(ctx, wf.Name, wf.Annotations[runIDAnnotation], io.Discard)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("native wait expired before caller stopped observing: %v", err)
		}
		if _, err := workflows.Get(t.Context(), wf.Name, metav1.GetOptions{}); err != nil {
			t.Fatal("waiting Workflow was deleted", err)
		}
	}
}
