package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	argofake "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned/fake"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argowatch"
	"github.com/oberthci/oberth/internal/model"
)

type watchSchedulingObserver struct{ controller *argojob.Controller }

func (o watchSchedulingObserver) ObserveScheduling(ctx context.Context, run model.Run) (*model.ExecutionObservation, error) {
	return o.controller.ObserveScheduling(ctx, run.WorkflowName, run.ID, run.SHA)
}
func (o watchSchedulingObserver) ObserveSynchronization(ctx context.Context, run model.Run) (*model.SyncObservation, error) {
	return o.controller.ObserveSynchronization(ctx, run.WorkflowName, run.ID)
}

func TestWatchHubRunListAndSweepsMakeNoAPIReads(t *testing.T) {
	f := newAgentFixture(t)
	const namespace = "pipeline"
	var active []model.Run
	var workflows, pods []runtime.Object
	// More than the old 64-run sweep budget: every run must be observed.
	for i := range 65 {
		run := submittedDisplayRun(t, f.controlFixture, fmt.Sprintf("watch-%d", i))
		active = append(active, run)
		uid := types.UID(run.WorkflowName + "-uid")
		workflows = append(workflows, &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: run.WorkflowName, Namespace: namespace, UID: uid, Labels: map[string]string{"oberth.ci/engine": "argo", "oberth.ci/sha": run.SHA}, Annotations: map[string]string{"oberth.ci/run-id": run.ID}}, Status: wfv1.WorkflowStatus{Phase: wfv1.WorkflowRunning}})
		pods = append(pods, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: run.WorkflowName + "-step", Namespace: namespace, Labels: map[string]string{"oberth.ci/trigger": "push", "workflows.argoproj.io/workflow": run.WorkflowName}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: run.WorkflowName, UID: uid, Controller: ptr.To(true)}}}, Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "Insufficient cpu PRIVATE"}}}})
	}
	kube := kubefake.NewClientset(pods...)
	argo := argofake.NewSimpleClientset(workflows...)
	argo.PrependWatchReactor("workflows", func(action clienttesting.Action) (bool, watch.Interface, error) {
		options := action.(interface{ GetListOptions() metav1.ListOptions }).GetListOptions()
		if options.SendInitialEvents != nil && *options.SendInitialEvents {
			return true, nil, apierrors.NewBadRequest("streaming lists unsupported")
		}
		return false, nil, nil
	})
	hub, err := argowatch.New(kube, argo, namespace, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); hub.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	syncCtx, syncCancel := context.WithTimeout(ctx, 10*time.Second)
	defer syncCancel()
	if err := hub.WaitForSync(syncCtx); err != nil {
		t.Fatal(err)
	}
	// Build needs administrator identity defaults; observations only need the namespace.
	controller, err := argojob.NewController(argo.ArgoprojV1alpha1().Workflows(namespace), kube, argojob.Config{Namespace: namespace, PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "credentialed", CISecretsServiceAccount: "ci-secrets", ExecutorServiceAccount: "executor"})
	if err != nil {
		t.Fatal(err)
	}
	controller.WithWatchHub(hub)
	f.service.schedulingObserver = watchSchedulingObserver{controller}
	beforeKube, beforeArgo := len(kube.Actions()), len(argo.Actions())
	for range 3 {
		value, err := f.service.Runs(ctx, api.Actor{Identity: "owner"}, api.RunFilter{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		rows := value.([]model.Run)
		if len(rows) != 65 {
			t.Fatal("missing rows", len(rows))
		}
		for _, run := range rows {
			if !strings.HasPrefix(run.DisplayReason, "pod pending: insufficient cpu") {
				t.Fatal(run.DisplayReason)
			}
		}
		f.recorder.observeActive(ctx, active)
		if f.recorder.snapshot.Unobserved != 0 || len(f.recorder.snapshot.Participants) != 65 {
			t.Fatal(f.recorder.snapshot)
		}
	}
	if len(kube.Actions()) != beforeKube || len(argo.Actions()) != beforeArgo {
		t.Fatal("run list or agent sweep performed API reads", kube.Actions()[beforeKube:], argo.Actions()[beforeArgo:])
	}
}
