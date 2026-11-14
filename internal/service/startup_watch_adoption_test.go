package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	argofake "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/oberthci/oberth/internal/argowatch"
	"github.com/oberthci/oberth/internal/model"
)

type startupWatchJobs struct {
	*adoptingJobs
	syncWatch func(context.Context) error
}

func (j *startupWatchJobs) WaitForWatchSync(ctx context.Context) error {
	if j.syncWatch != nil {
		return j.syncWatch(ctx)
	}
	return ctx.Err()
}

func TestStartupWatchAdoptionSettlesCompletedAndDispatchesRunning(t *testing.T) {
	for _, kind := range []string{"running", "completed", "confirmed-miss"} {
		t.Run(kind, func(t *testing.T) {
			f := newRecoveryFixture(t)
			run := f.enqueueAndClaim(t, "feature/"+kind, strings.Repeat("a", 40))
			f.setWorkflowName(t, run.ID, "original-workflow")
			jobs := &startupWatchJobs{adoptingJobs: &adoptingJobs{found: kind != "confirmed-miss"}}
			scheduler := f.scheduler(t, jobs)
			dispatched := 0
			if err := scheduler.reconcileStrandedRunsWithGate(t.Context(), scheduler.requireMutation, func(model.Run) error { dispatched++; return nil }); err != nil {
				t.Fatal(err)
			}
			current, err := f.store.Run(t.Context(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if kind != "confirmed-miss" {
				if dispatched != 1 || current.Status != model.RunRunning {
					t.Fatal("running adoption was not dispatched", current, dispatched)
				}
			} else {
				if dispatched != 0 || current.Status != model.RunFailed {
					t.Fatal("terminal/absent Workflow was dispatched", current, dispatched)
				}
			}
			if kind == "confirmed-miss" && current.Error != model.ServerRestartReason {
				t.Fatal("missing Workflow lost B4 reason", current)
			}
			if jobs.created != 0 || jobs.releaseCreated != 0 || len(jobs.deleted) != 0 {
				t.Fatal("startup resubmitted or deleted Workflow", jobs)
			}
		})
	}
}

func TestStartupWatchAdoptionWaitsThroughBootOutage(t *testing.T) {
	for _, recoverAPI := range []bool{false, true} {
		t.Run(map[bool]string{false: "shutdown", true: "recovery"}[recoverAPI], func(t *testing.T) {
			f := newRecoveryFixture(t)
			run := f.enqueueAndClaim(t, "feature/boot", strings.Repeat("a", 40))
			f.setWorkflowName(t, run.ID, "original-workflow")
			argo := argofake.NewSimpleClientset(&wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "original-workflow", Namespace: "pipeline", UID: types.UID("native-uid"), Labels: map[string]string{"oberth.ci/engine": "argo"}, Annotations: map[string]string{"oberth.ci/run-id": run.ID}}, Status: wfv1.WorkflowStatus{Phase: wfv1.WorkflowRunning}})
			kube := kubefake.NewClientset()
			var unavailable atomic.Bool
			unavailable.Store(true)
			attempt := make(chan struct{}, 1)
			argo.PrependWatchReactor("workflows", func(action clienttesting.Action) (bool, watch.Interface, error) {
				options := action.(interface{ GetListOptions() metav1.ListOptions }).GetListOptions()
				if options.SendInitialEvents != nil && *options.SendInitialEvents {
					return true, nil, apierrors.NewBadRequest("streaming lists unsupported")
				}
				return false, nil, nil
			})
			argo.PrependReactor("list", "workflows", func(clienttesting.Action) (bool, runtime.Object, error) {
				if unavailable.Load() {
					select {
					case attempt <- struct{}{}:
					default:
					}
					return true, nil, errors.New("API unavailable")
				}
				return false, nil, nil
			})
			hub, err := argowatch.New(kube, argo, "pipeline", nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			watchDone := make(chan struct{})
			go func() { defer close(watchDone); hub.Run(ctx) }()
			t.Cleanup(func() { cancel(); <-watchDone })
			jobs := &startupWatchJobs{adoptingJobs: &adoptingJobs{}, syncWatch: hub.WaitForSync}
			jobs.adoptHook = func(_ context.Context, r model.Run) (bool, error) {
				_, found := hub.Workflow(r.WorkflowName)
				return found, nil
			}
			scheduler := f.scheduler(t, jobs)
			done := make(chan error, 1)
			var dispatched atomic.Int32
			go func() {
				done <- scheduler.reconcileStrandedRunsWithGate(ctx, scheduler.requireMutation, func(model.Run) error { dispatched.Add(1); return nil })
			}()
			select {
			case <-attempt:
			case <-ctx.Done():
				t.Fatal("boot outage was not observed")
			}
			select {
			case err := <-done:
				t.Fatal("startup ended before sync", err)
			default:
			}
			current, err := f.store.Run(t.Context(), run.ID)
			if err != nil || current.Status != model.RunRunning || current.Error != "" {
				t.Fatal("boot outage failed run", current, err)
			}
			jobs.mu.Lock()
			adopted := len(jobs.adopted)
			jobs.mu.Unlock()
			if adopted != 0 || dispatched.Load() != 0 {
				t.Fatal("recovery used unsynced cache")
			}
			if recoverAPI {
				unavailable.Store(false)
			} else {
				cancel()
			}
			select {
			case err = <-done:
				if recoverAPI && err != nil {
					t.Fatal(err)
				}
				if !recoverAPI && !errors.Is(err, context.Canceled) {
					t.Fatal("shutdown did not unblock sync", err)
				}
			case <-ctx.Done():
				if recoverAPI {
					t.Fatal("startup did not recover")
				}
				err = <-done
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			}
			current, err = f.store.Run(t.Context(), run.ID)
			if err != nil || current.Status != model.RunRunning || (recoverAPI && dispatched.Load() != 1) || jobs.created != 0 || len(jobs.deleted) != 0 {
				t.Fatal("recovery changed native execution", current, err, dispatched.Load())
			}
		})
	}
}

func TestStartupWatchAdoptionRetriesUncertainLiveRead(t *testing.T) {
	f := newRecoveryFixture(t)
	run := f.enqueueAndClaim(t, "feature/read-recovery", strings.Repeat("a", 40))
	f.setWorkflowName(t, run.ID, "original-workflow")
	jobs := &startupWatchJobs{adoptingJobs: &adoptingJobs{}}
	var reads atomic.Int32
	jobs.adoptHook = func(context.Context, model.Run) (bool, error) {
		if reads.Add(1) == 1 {
			return false, apierrors.NewServiceUnavailable("API unavailable")
		}
		return true, nil
	}
	scheduler := f.scheduler(t, jobs)
	dispatched := 0
	if err := scheduler.reconcileStrandedRunsWithGate(t.Context(), scheduler.requireMutation, func(model.Run) error { dispatched++; return nil }); err != nil {
		t.Fatal(err)
	}
	current, err := f.store.Run(t.Context(), run.ID)
	if err != nil || current.Status != model.RunRunning || dispatched != 1 || reads.Load() != 2 || jobs.created != 0 || len(jobs.deleted) != 0 {
		t.Fatal("uncertain lookup changed run", current, err, reads.Load())
	}
}

func TestStartupWatchAdoptionRetriesOnlyUnavailableErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		retry bool
	}{
		{"watch", ErrUnavailable, true},
		{"deadline", context.DeadlineExceeded, true},
		{"throttled", apierrors.NewTooManyRequests("private", 1), true},
		{"server", &apierrors.StatusError{ErrStatus: metav1.Status{Code: 502}}, true},
		{"timeout", apierrors.NewTimeoutError("private", 1), true},
		{"transport", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, true},
		{"eof", io.ErrUnexpectedEOF, true},
		{"forbidden", apierrors.NewForbidden(schema.GroupResource{Resource: "workflows"}, "workflow", errors.New("private")), false},
		{"unauthorized", apierrors.NewUnauthorized("private"), false},
		{"bad-trigger", errors.New("invalid run trigger"), false},
		{"no-adopter", errors.New("controller cannot adopt"), false},
		{"refused", ErrAdoptionRefused, false},
		{"canceled", context.Canceled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jobs := &startupWatchJobs{adoptingJobs: &adoptingJobs{}}
			calls := 0
			jobs.adoptHook = func(context.Context, model.Run) (bool, error) {
				calls++
				if calls == 1 {
					return false, fmt.Errorf("wrapped: %w", tc.err)
				}
				return true, nil
			}
			scheduler := &Scheduler{}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			found, err := scheduler.adoptStartupWorkflow(ctx, jobs, model.Run{ID: "run", WorkflowName: "workflow"})
			if tc.retry {
				if err != nil || !found || calls != 2 {
					t.Fatal(found, err, calls)
				}
			} else if !errors.Is(err, tc.err) || found || calls != 1 {
				t.Fatal(found, err, calls)
			}
			class, retry := startupAdoptionErrorClass(fmt.Errorf("wrapped: %w", tc.err))
			if class == "" || retry != tc.retry || strings.Contains(class, "private") {
				t.Fatal(class, retry)
			}
		})
	}
}

func TestStartupWatchAdoptionSkipsCancelledRuns(t *testing.T) {
	f := newRecoveryFixture(t)
	run := f.enqueueAndClaim(t, "feature/cancelled", strings.Repeat("a", 40))
	f.setWorkflowName(t, run.ID, "original-workflow")
	if _, err := f.store.CancelRun(t.Context(), model.RunCancelRequest{RunID: run.ID, Actor: "agent@host"}); err != nil {
		t.Fatal(err)
	}
	rows, err := f.store.RunningRunsWithJobs(t.Context())
	if err != nil || len(rows) != 1 || rows[0].ID != run.ID || rows[0].Status != model.RunRunning || rows[0].Phase != "cancelled" {
		t.Fatal("fixture must remain in startup adoption query", rows, err)
	}
	jobs := &startupWatchJobs{adoptingJobs: &adoptingJobs{found: true}}
	scheduler := f.scheduler(t, jobs)
	dispatched := 0
	if err := scheduler.reconcileStrandedRunsWithGate(t.Context(), scheduler.requireMutation, func(model.Run) error { dispatched++; return nil }); err != nil {
		t.Fatal(err)
	}
	current, err := f.store.Run(t.Context(), run.ID)
	if err != nil || current.Status != model.RunRunning || !current.Cancelled() || dispatched != 0 || len(jobs.adopted) != 0 || len(jobs.deleted) != 0 {
		t.Fatal(current, err, dispatched, jobs.adopted)
	}
}
