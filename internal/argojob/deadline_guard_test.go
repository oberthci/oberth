package argojob

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	wffake "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestDeadlineGuardMissingDocumentDeadline(t *testing.T) {
	source := strings.Replace(greedyDocument, "  activeDeadlineSeconds: 999999\n", "", 1)
	wf, err := Build(testConfig(), testRequest(pipeline.TriggerCI, source))
	if err != nil {
		t.Fatal(err)
	}
	if wf.Spec.ActiveDeadlineSeconds == nil || *wf.Spec.ActiveDeadlineSeconds != 1800 {
		t.Fatalf("missing default 30-minute deadline: %v", wf.Spec.ActiveDeadlineSeconds)
	}
}
func TestDeadlineGuardUnstartedWorkflowWatchdog(t *testing.T) {
	workflow := schedulingWorkflow()
	workflow.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	workflow.Status.StartedAt = metav1.Time{}
	workflow.Spec.ActiveDeadlineSeconds = ptr.To(int64(1))
	workflow.Status.Phase = wfv1.WorkflowRunning
	config := testConfig()
	workflow.Namespace = config.Namespace
	workflows := wffake.NewSimpleClientset(workflow).ArgoprojV1alpha1().Workflows(config.Namespace)
	controller, err := NewController(workflows, fake.NewClientset(), config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err = controller.Wait(ctx, workflow.Name, workflow.Annotations[runIDAnnotation], io.Discard)
	if err == nil || !strings.Contains(err.Error(), model.ExecutionDeadlineMessage) {
		t.Fatalf("watchdog did not expire unstarted workflow: %v", err)
	}
	if _, err := workflows.Get(t.Context(), workflow.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("watchdog left workflow running: %v", err)
	}
}

func TestDeadlineGuardRepositoryCapsAndShorterDocuments(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cap      time.Duration
		declared string
		want     int64
	}{
		{"repo cap", 5 * time.Minute, "999999", 300},
		{"shorter document", 5 * time.Minute, "60", 60},
		{"cap cannot raise global", 8 * time.Hour, "999999", 7200},
		{"default below cap", time.Hour, "", 1800},
		{"missing below cap", time.Minute, "", 60},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := testConfig()
			source := strings.Replace(greedyDocument, "999999", tc.declared, 1)
			if tc.declared == "" {
				source = strings.Replace(greedyDocument, "  activeDeadlineSeconds: 999999\n", "", 1)
			}
			request := testRequest(pipeline.TriggerCI, source)
			request.UpstreamName = "codeberg"
			config.PerRepoWorkflowTimeout = map[string]time.Duration{canonicalRepoKey(request.UpstreamName, request.UpstreamOrg, request.Repo): tc.cap}
			got, err := Build(config, request)
			if err != nil {
				t.Fatal(err)
			}
			if *got.Spec.ActiveDeadlineSeconds != tc.want {
				t.Fatalf("deadline %d, want %d", *got.Spec.ActiveDeadlineSeconds, tc.want)
			}
			request.UpstreamName = "another"
			other, err := Build(config, request)
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "repo cap" && *other.Spec.ActiveDeadlineSeconds != 7200 {
				t.Fatal("repository cap leaked to another upstream")
			}
		})
	}
}
