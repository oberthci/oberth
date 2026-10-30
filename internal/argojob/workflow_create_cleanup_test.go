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
)

type cleanupWorkflowClient struct {
	createRetryWorkflowClient
	deletes    []metav1.DeleteOptions
	deleteHook func(context.Context, metav1.DeleteOptions) error
}

func (c *cleanupWorkflowClient) Delete(ctx context.Context, _ string, opts metav1.DeleteOptions) error {
	c.deletes = append(c.deletes, opts)
	if c.deleteHook != nil {
		return c.deleteHook(ctx, opts)
	}
	return nil
}

func TestWorkflowCreateCleanupHoldsThroughLateCommitAndFinalAbsence(t *testing.T) {
	for _, mode := range []string{"absent", "late", "second-late", "foreign", "uid-conflict", "stuck", "final-read-error", "blocked-reads", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				wf := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "wf", UID: "late-uid", Annotations: map[string]string{runIDAnnotation: "run", identityAnnotation: "spec"}}}
				client := &cleanupWorkflowClient{}
				client.getHook = func(ctx context.Context, name string) (*wfv1.Workflow, error) {
					elapsed := time.Since(start)
					if mode == "blocked-reads" {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					if mode == "final-read-error" && elapsed >= 60*time.Second {
						return nil, apierrors.NewServiceUnavailable("unavailable")
					}
					if mode == "foreign" && elapsed >= 20*time.Second {
						foreign := wf.DeepCopy()
						foreign.Annotations[runIDAnnotation] = "other"
						return foreign, nil
					}
					visible := mode == "late" || mode == "second-late" || mode == "uid-conflict" || mode == "stuck"
					if visible && elapsed >= 20*time.Second && (len(client.deletes) == 0 || mode == "stuck") {
						return wf.DeepCopy(), nil
					}
					if mode == "second-late" && elapsed >= 45*time.Second && len(client.deletes) == 1 {
						second := wf.DeepCopy()
						second.UID = "second-uid"
						return second, nil
					}
					return nil, apierrors.NewNotFound(workflowTestResource, name)
				}
				client.deleteHook = func(_ context.Context, opts metav1.DeleteOptions) error {
					if opts.Preconditions == nil || opts.Preconditions.UID == nil || *opts.Preconditions.UID == "" || opts.PropagationPolicy == nil || *opts.PropagationPolicy != metav1.DeletePropagationForeground {
						t.Fatal("deletion not UID-fenced/foreground")
					}
					if mode == "uid-conflict" {
						return apierrors.NewConflict(workflowTestResource, "wf", errors.New("UID changed"))
					}
					return nil
				}
				ctx := t.Context()
				if mode == "cancel" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 7*time.Second)
					defer cancel()
				}
				err := (&Controller{workflows: client}).SettleCreate(ctx, "wf", "run")
				switch mode {
				case "absent", "late", "second-late":
					expected := 0
					if mode == "late" {
						expected = 1
					}
					if mode == "second-late" {
						expected = 2
					}
					if err != nil || time.Since(start) != 60*time.Second || len(client.deletes) != expected {
						t.Fatalf("released without window/final proof: elapsed=%s deletes=%d err=%v", time.Since(start), len(client.deletes), err)
					}
					if expected > 0 && *client.deletes[0].Preconditions.UID != "late-uid" {
						t.Fatal("wrong Workflow UID deleted")
					}
					if expected == 2 && *client.deletes[1].Preconditions.UID != "second-uid" {
						t.Fatal("late replacement not fenced separately")
					}
				case "foreign":
					if err == nil || !strings.Contains(err.Error(), "belongs to run") || len(client.deletes) != 0 {
						t.Fatalf("foreign object deleted: %v", err)
					}
				case "uid-conflict":
					if !apierrors.IsConflict(err) || len(client.deletes) != 1 {
						t.Fatalf("UID conflict treated as success: %v", err)
					}
				case "cancel":
					if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 7*time.Second {
						t.Fatalf("cancellation ignored: elapsed=%s err=%v", time.Since(start), err)
					}
				default:
					if err == nil || time.Since(start) > 65*time.Second || time.Since(start) < 60*time.Second {
						t.Fatalf("unknown/still-present outcome freed or escaped budget: elapsed=%s err=%v", time.Since(start), err)
					}
				}
				if len(client.posts) != 0 {
					t.Fatal("cleanup submitted a Workflow")
				}
			})
		})
	}
}
