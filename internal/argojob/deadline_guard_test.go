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
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestDeadlineGuardMissingDocumentDeadline(t *testing.T) {
	source := strings.Replace(greedyDocument, "  activeDeadlineSeconds: 999999\n", "", 1)
	wf, err := Build(testConfig(), testRequest(periapsis.TriggerCI, source))
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
			config.PerRepoWorkflowTimeout = map[string]time.Duration{"codeberg/skipops/oberth": tc.cap}
			source := strings.Replace(greedyDocument, "999999", tc.declared, 1)
			if tc.declared == "" {
				source = strings.Replace(greedyDocument, "  activeDeadlineSeconds: 999999\n", "", 1)
			}
			request := testRequest(periapsis.TriggerCI, source)
			request.UpstreamName = "codeberg"
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

func TestDeadlineGuardTrustedCancellationKeepsAbsenceObligation(t *testing.T) {
	config, request, workflow, pod := trustedKVMObservedFixture(t)
	client := &trustedCleanupClient{workflow: workflow}
	controller := &Controller{config: config, workflows: client, kube: trustedControllerClient(pod)}
	if err := controller.CancelTrustedKVM(t.Context(), *request.TrustedKVM, "foreign"); err == nil || client.deleted {
		t.Fatal("foreign UID cancelled")
	}
	if err := controller.CancelTrustedKVM(t.Context(), *request.TrustedKVM, string(workflow.UID)); err != nil {
		t.Fatal(err)
	}
	if !client.deleted {
		t.Fatal("trusted workflow was not targeted for deletion")
	}
}

func TestDeadlineGuardVMWithoutDeclaredDeadline(t *testing.T) {
	source := strings.Replace(kubeVirtWorkflow, "  activeDeadlineSeconds: 3600\n", "", 1)
	_, workflow, _ := guardedVMISourceFixture(t, 1, 7, source)
	if workflow.Spec.ActiveDeadlineSeconds == nil || *workflow.Spec.ActiveDeadlineSeconds != 1800 {
		t.Fatalf("VM has no default deadline: %v", workflow.Spec.ActiveDeadlineSeconds)
	}
	origin := time.Now().Add(-time.Hour).UTC()
	workflow.CreationTimestamp = metav1.NewTime(origin)
	observation := &model.ExecutionObservation{}
	observeServerDeadline(workflow, observation, 30*time.Minute, origin.Add(time.Minute))
	if observation.ServerDeadline == nil || observation.ServerDeadline.ExpiresAt == nil || !observation.ServerDeadline.ExpiresAt.Equal(origin.Add(33*time.Minute)) || *observation.ServerDeadline.RemainingSeconds != 1920 {
		t.Fatalf("VM watchdog hidden or reset: %+v", observation)
	}
}
