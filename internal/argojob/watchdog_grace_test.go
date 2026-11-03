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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/model"
)

func TestDeadlineGuardArgoCompletesInsideWatchdogGrace(t *testing.T) {
	config := testConfig()
	workflow := schedulingWorkflow()
	workflow.Namespace = config.Namespace
	workflow.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	workflow.Status.StartedAt = metav1.NewTime(time.Now().Add(-2 * time.Second))
	// The older Workflow has real scheduled work; this test isolates its execution watchdog.
	workflow.Status.Nodes["step"] = wfv1.NodeStatus{ID: "step", Type: wfv1.NodeTypePod, Phase: wfv1.NodeRunning, HostNodeName: "worker"}
	workflow.Spec.ActiveDeadlineSeconds = ptr.To(int64(1))
	client := wffake.NewSimpleClientset(workflow)
	reads := 0
	client.PrependReactor("get", "workflows", func(ktesting.Action) (bool, runtime.Object, error) {
		reads++
		observed := workflow.DeepCopy()
		if reads > 1 {
			observed.Status.Phase = wfv1.WorkflowFailed
			observed.Status.Message = "Workflow deadline exceeded"
			observed.Status.Nodes["step"] = wfv1.NodeStatus{ID: "step", Name: "wf.test", DisplayName: "test", Type: wfv1.NodeTypePod, Phase: wfv1.NodeFailed, Message: "Step exceeded its deadline"}
		}
		return true, observed, nil
	})
	controller, err := NewController(client.ArgoprojV1alpha1().Workflows(config.Namespace), fake.NewClientset(), config)
	if err != nil {
		t.Fatal(err)
	}
	controller.interval = time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	completion, err := controller.Wait(ctx, workflow.Name, "run", io.Discard)
	if err != nil || !completion.Terminal || completion.Reason != model.ExecutionDeadlineMessage || len(completion.Steps) != 1 || completion.FailedStep != "test" {
		t.Fatalf("Argo deadline evidence lost inside grace: %+v, %v", completion, err)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "delete" {
			t.Fatal("watchdog deleted Workflow before Argo deadline observation")
		}
	}
}

func TestDeadlineGuardServerObservationUsesStartAndGrace(t *testing.T) {
	for _, budget := range []time.Duration{time.Minute, time.Hour} {
		t.Run(budget.String(), func(t *testing.T) {
			workflow := schedulingWorkflow()
			created := time.Now().UTC().Truncate(time.Second).Add(-2 * time.Hour)
			started := created.Add(time.Hour)
			workflow.CreationTimestamp = metav1.NewTime(created)
			workflow.Status.StartedAt = metav1.NewTime(started)
			result := &model.ExecutionObservation{}
			observeServerDeadline(workflow, result, budget, started.Add(budget))
			want := started.Add(budget + max(2*time.Minute, budget/10))
			if got := result.ServerDeadline; got.Clock != "workflow_started_at" || got.StartedAt == nil || !got.StartedAt.Equal(started) || !got.ExpiresAt.Equal(want) || got.Elapsed {
				t.Fatalf("watchdog did not leave Argo time to report: %+v; want expiry %s", got, want)
			}
		})
	}
}

func TestDeadlineGuardWatchdogRebindsToLateStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		config := testConfig()
		workflow := schedulingWorkflow()
		workflow.Namespace = config.Namespace
		begin := time.Now()
		workflow.CreationTimestamp = metav1.NewTime(begin)
		workflow.Spec.ActiveDeadlineSeconds = ptr.To(int64(1))
		client := wffake.NewSimpleClientset(workflow)
		client.PrependReactor("get", "workflows", func(ktesting.Action) (bool, runtime.Object, error) {
			observed := workflow.DeepCopy()
			if time.Since(begin) >= 3*time.Second {
				observed.Status.StartedAt = metav1.NewTime(begin.Add(3 * time.Second))
			}
			return true, observed, nil
		})
		controller, err := NewController(client.ArgoprojV1alpha1().Workflows(config.Namespace), fake.NewClientset(), config)
		if err != nil {
			t.Fatal(err)
		}
		controller.interval = 3 * time.Second
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
		defer cancel()
		_, err = controller.Wait(ctx, workflow.Name, "run", io.Discard)
		if err == nil || !strings.Contains(err.Error(), "Server watchdog expired") || time.Since(begin) != 124*time.Second {
			t.Fatalf("watchdog did not expire at original start + budget + grace: elapsed=%s, err=%v", time.Since(begin), err)
		}
	})
}

func TestDeadlineGuardFirstReadOutageDoesNotUseDefaultBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		config := testConfig()
		workflow := schedulingWorkflow()
		workflow.Namespace = config.Namespace
		begin := time.Now()
		workflow.CreationTimestamp = metav1.NewTime(begin)
		workflow.Spec.ActiveDeadlineSeconds = ptr.To(int64(6 * 60 * 60))
		workflow.Status.Phase = wfv1.WorkflowSucceeded
		client := wffake.NewSimpleClientset(workflow)
		client.PrependReactor("get", "workflows", func(ktesting.Action) (bool, runtime.Object, error) {
			if time.Since(begin) < 31*time.Minute {
				return true, nil, apierrors.NewServiceUnavailable("read unavailable")
			}
			return true, workflow.DeepCopy(), nil
		})
		controller, err := NewController(client.ArgoprojV1alpha1().Workflows(config.Namespace), fake.NewClientset(), config)
		if err != nil {
			t.Fatal(err)
		}
		controller.interval, controller.readGrace = time.Minute, time.Hour
		ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
		defer cancel()
		completion, err := controller.Wait(ctx, workflow.Name, "run", io.Discard)
		if err != nil || !completion.Succeeded || time.Since(begin) != 31*time.Minute {
			t.Fatalf("first read outage substituted default deadline: %+v, elapsed=%s, err=%v", completion, time.Since(begin), err)
		}
		for _, action := range client.Actions() {
			if action.GetVerb() == "delete" {
				t.Fatal("first read outage deleted longer-deadline Workflow")
			}
		}
	})
}
