package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
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

// Seed Pods deliberately remain outside the step-Pod cache. An unresponsive
// exact GET must have a short local deadline even on an otherwise healthy hub.
func TestRunListBlockedSeedReadsReturnPromptly(t *testing.T) {
	f := newControlFixture(t)
	for i := range 3 {
		submittedDisplayRun(t, f, fmt.Sprintf("blocked-seed-%d", i))
	}
	var gets atomic.Int32
	seedAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.Contains(r.URL.Path, "/pods/") {
			t.Errorf("unexpected seed operation %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			return
		}
		gets.Add(1)
		<-r.Context().Done()
	}))
	defer seedAPI.Close()
	kube, err := kubernetes.NewForConfig(&rest.Config{Host: seedAPI.URL})
	if err != nil {
		t.Fatal(err)
	}
	config := argojob.Config{Namespace: "pipeline", PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "credentialed", CISecretsServiceAccount: "ci-secrets", ExecutorServiceAccount: "executor"}
	argo := argofake.NewSimpleClientset()
	controller, err := argojob.NewController(argo.ArgoprojV1alpha1().Workflows(config.Namespace), kube, config)
	if err != nil {
		t.Fatal(err)
	}
	seeder := argojob.NewSourceSeeder(kube, func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
		return fmt.Errorf("unexpected seed exec")
	}, config)
	controller.WithWatchHub(emptyHealthyWatchHub{}).WithSourceSeeder(seeder)
	s := f.api(t)
	s.schedulingObserver = watchSchedulingObserver{controller}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	start := time.Now()
	value, err := s.Runs(ctx, api.Actor{Identity: "owner"}, api.RunFilter{})
	elapsed := time.Since(start)
	if err != nil || elapsed > 750*time.Millisecond {
		t.Fatal("blocked seed GET held the list", elapsed, err)
	}
	rows := value.([]model.Run)
	if len(rows) != 3 || gets.Load() != 3 {
		t.Fatal("seed fallback not exercised", len(rows), gets.Load())
	}
	for _, run := range rows {
		if run.Scheduling == nil || run.Scheduling.Execution == nil || run.Scheduling.Execution.State != "unavailable" || run.DisplayReason != "awaiting Argo state" {
			t.Fatal(run)
		}
	}
	if len(argo.Actions()) != 0 {
		t.Fatal("Workflow cache miss polled Argo", argo.Actions())
	}
}

type emptyHealthyWatchHub struct{}

func (emptyHealthyWatchHub) Workflow(string) (*wfv1.Workflow, bool) { return nil, false }
func (emptyHealthyWatchHub) PodsOf(string) []*corev1.Pod            { return nil }
func (emptyHealthyWatchHub) Pods() []*corev1.Pod                    { return nil }
func (emptyHealthyWatchHub) Healthy() bool                          { return true }
func (emptyHealthyWatchHub) Synced() bool                           { return true }
func (emptyHealthyWatchHub) UnhealthySince() (time.Time, bool)      { return time.Time{}, false }

func TestRunListBeforeInitialSyncAwaitsArgoWithoutOutageClaim(t *testing.T) {
	f := newControlFixture(t)
	submittedDisplayRun(t, f, "initial-watch-sync")
	kube := kubefake.NewClientset()
	argo := argofake.NewSimpleClientset()
	hub, err := argowatch.New(kube, argo, "pipeline", nil)
	if err != nil {
		t.Fatal(err)
	}
	config := argojob.Config{Namespace: "pipeline", PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "credentialed", CISecretsServiceAccount: "ci-secrets", ExecutorServiceAccount: "executor"}
	controller, err := argojob.NewController(argo.ArgoprojV1alpha1().Workflows(config.Namespace), kube, config)
	if err != nil {
		t.Fatal(err)
	}
	controller.WithWatchHub(hub)
	s := f.api(t)
	s.schedulingObserver = watchSchedulingObserver{controller}
	value, err := s.Runs(t.Context(), api.Actor{Identity: "owner"}, api.RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	rows := value.([]model.Run)
	if len(rows) != 1 || rows[0].DisplayReason != "awaiting Argo state" || strings.Contains(rows[0].Scheduling.Execution.Message, "unreachable") {
		t.Fatal(rows)
	}
	if hub.Synced() || len(kube.Actions()) != 0 || len(argo.Actions()) != 0 {
		t.Fatal("startup observation used live API")
	}
}

func (emptyHealthyWatchHub) Subscribe(string) (<-chan struct{}, func()) {
	return make(chan struct{}, 1), func() {}
}
