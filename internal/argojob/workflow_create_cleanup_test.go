package argojob

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type cleanupWorkflowClient struct {
	createRetryWorkflowClient
	patches   int
	patchHook func(context.Context, types.PatchType, []byte) (*wfv1.Workflow, error)
}

func (c *cleanupWorkflowClient) Patch(ctx context.Context, _ string, kind types.PatchType, data []byte, _ metav1.PatchOptions, _ ...string) (*wfv1.Workflow, error) {
	c.patches++
	if c.patchHook != nil {
		return c.patchHook(ctx, kind, data)
	}
	return nil, errors.New("unexpected Stop patch")
}

func TestWorkflowCreateCleanupOneBoundedObservation(t *testing.T) {
	for _, mode := range []string{"early-absence", "final-absence", "found", "stopped", "completed", "deleting", "foreign", "uid-conflict", "blocked-read", "blocked-patch", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				boundary := time.Now().Add(time.Minute)
				workflow := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "wf", UID: "owned-uid", ResourceVersion: "1", Annotations: map[string]string{runIDAnnotation: "run"}}, Status: wfv1.WorkflowStatus{Phase: wfv1.WorkflowRunning}}
				if mode == "stopped" {
					workflow.Spec.Shutdown = wfv1.ShutdownStrategyStop
				}
				if mode == "completed" {
					workflow.Status.Phase = wfv1.WorkflowFailed
				}
				if mode == "foreign" {
					workflow.Annotations[runIDAnnotation] = "foreign"
				}
				if mode == "deleting" {
					stamp := metav1.Now()
					workflow.DeletionTimestamp = &stamp
				}
				client := &cleanupWorkflowClient{}
				reads := 0
				client.getHook = func(ctx context.Context, name string) (*wfv1.Workflow, error) {
					reads++
					if mode == "blocked-read" {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					if mode == "early-absence" || mode == "final-absence" {
						return nil, apierrors.NewNotFound(workflowTestResource, name)
					}
					return workflow.DeepCopy(), nil
				}
				client.patchHook = func(ctx context.Context, kind types.PatchType, data []byte) (*wfv1.Workflow, error) {
					if mode == "blocked-patch" || mode == "cancel" {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					if mode == "uid-conflict" {
						return nil, apierrors.NewConflict(workflowTestResource, "wf", errors.New("UID changed"))
					}
					return applyWorkflowStopTestPatch(workflow, kind, data)
				}
				if mode == "final-absence" {
					time.Sleep(time.Minute)
				}
				ctx := t.Context()
				if mode == "cancel" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 3*time.Second)
					defer cancel()
				}
				started := time.Now()
				done, err := (&Controller{workflows: client}).SettleCreate(ctx, "wf", "run", boundary)
				if reads != 1 || len(client.posts) != 0 || time.Since(started) > 10*time.Second {
					t.Fatalf("observation retried/escaped budget: %d %v", reads, err)
				}
				switch mode {
				case "final-absence", "completed":
					if !done || err != nil {
						t.Fatal("terminal/definite absence not settled", done, err)
					}
				case "early-absence", "found", "stopped", "deleting":
					if done || err != nil {
						t.Fatal("Stop acceptance became terminal evidence", done, err)
					}
				case "foreign":
					if done || err == nil || !strings.Contains(err.Error(), "belongs to run") || client.patches != 0 {
						t.Fatal("foreign Workflow mutated", err)
					}
				case "uid-conflict":
					if done || !apierrors.IsConflict(err) {
						t.Fatal("conflict released custody", err)
					}
				default:
					if done || !errors.Is(err, context.DeadlineExceeded) {
						t.Fatal("observation/Stop lost deadline", done, err)
					}
				}
				if (mode == "completed" || mode == "stopped" || mode == "deleting") && client.patches != 0 {
					t.Fatal("terminal/already-stopped Workflow patched again")
				}
			})
		})
	}
}

func TestWorkflowCreateCleanupWaitsThroughExitHandlers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		boundary := time.Now().Add(time.Minute)
		workflow := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "wf", UID: "owned-uid", ResourceVersion: "1", Annotations: map[string]string{runIDAnnotation: "run"}}, Spec: wfv1.WorkflowSpec{OnExit: "cleanup"}, Status: wfv1.WorkflowStatus{Phase: wfv1.WorkflowRunning}}
		client := &cleanupWorkflowClient{}
		client.getHook = func(context.Context, string) (*wfv1.Workflow, error) { return workflow.DeepCopy(), nil }
		client.patchHook = func(_ context.Context, kind types.PatchType, data []byte) (*wfv1.Workflow, error) {
			var err error
			workflow, err = applyWorkflowStopTestPatch(workflow, kind, data)
			return workflow, err
		}
		controller := &Controller{workflows: client}
		for range 3 {
			done, err := controller.SettleCreate(t.Context(), "wf", "run", boundary)
			if done || err != nil {
				t.Fatal("running exit handler released custody", done, err)
			}
			time.Sleep(time.Minute)
		}
		if client.patches != 1 || workflow.Spec.OnExit != "cleanup" {
			t.Fatal("Stop repeated or exit handler changed")
		}
		workflow.Status.Phase = wfv1.WorkflowFailed
		if done, err := controller.SettleCreate(t.Context(), "wf", "run", boundary); !done || err != nil {
			t.Fatal("Argo completion did not settle", done, err)
		}
		if workflow.DeletionTimestamp != nil {
			t.Fatal("completed Workflow deleted instead of retained for TTL")
		}
	})
}

func TestWorkflowCreateCleanupEarlyReadCannotBecomeFinalByReturningLate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		boundary := time.Now().Add(60 * time.Second)
		client := &cleanupWorkflowClient{}
		client.getHook = func(context.Context, string) (*wfv1.Workflow, error) {
			time.Sleep(2 * time.Second)
			return nil, apierrors.NewNotFound(workflowTestResource, "wf")
		}
		time.Sleep(59 * time.Second)
		done, err := (&Controller{workflows: client}).SettleCreate(t.Context(), "wf", "run", boundary)
		if done || err != nil {
			t.Fatalf("pre-window observation became a final proof: %t %v", done, err)
		}
		done, err = (&Controller{workflows: client}).SettleCreate(t.Context(), "wf", "run", boundary)
		if !done || err != nil {
			t.Fatalf("post-window observation not accepted: %t %v", done, err)
		}
	})
}
