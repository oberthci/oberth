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

func TestWorkflowCreateCleanupOneBoundedObservation(t *testing.T) {
	for _, mode := range []string{"early-absence", "final-absence", "found", "deleting", "foreign", "uid-conflict", "blocked-read", "blocked-delete", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				boundary := start.Add(60 * time.Second)
				wf := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "wf", UID: "owned-uid", Annotations: map[string]string{runIDAnnotation: "run"}}}
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
					copy := wf.DeepCopy()
					if mode == "foreign" {
						copy.Annotations[runIDAnnotation] = "other"
					}
					if mode == "deleting" {
						stamp := metav1.Now()
						copy.DeletionTimestamp = &stamp
					}
					return copy, nil
				}
				client.deleteHook = func(ctx context.Context, opts metav1.DeleteOptions) error {
					if opts.Preconditions == nil || opts.Preconditions.UID == nil || *opts.Preconditions.UID != "owned-uid" || opts.PropagationPolicy == nil || *opts.PropagationPolicy != metav1.DeletePropagationForeground || opts.GracePeriodSeconds == nil || *opts.GracePeriodSeconds != 10 {
						t.Fatal("delete lost UID/foreground/10s grace fence")
					}
					if mode == "uid-conflict" {
						return apierrors.NewConflict(workflowTestResource, "wf", errors.New("UID changed"))
					}
					if mode == "blocked-delete" || mode == "cancel" {
						<-ctx.Done()
						return ctx.Err()
					}
					return nil
				}
				ctx := t.Context()
				if mode == "final-absence" {
					time.Sleep(60 * time.Second)
				}
				if mode == "cancel" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 3*time.Second)
					defer cancel()
				}
				callStart := time.Now()
				done, err := (&Controller{workflows: client}).SettleCreate(ctx, "wf", "run", boundary)
				if reads != 1 || len(client.posts) != 0 || time.Since(callStart) > 10*time.Second {
					t.Fatalf("attempt retried or escaped 10s: reads=%d elapsed=%s err=%v", reads, time.Since(callStart), err)
				}
				switch mode {
				case "final-absence":
					if !done || err != nil {
						t.Fatalf("final absence unconfirmed: %t %v", done, err)
					}
				case "early-absence", "found", "deleting":
					if done || err != nil || time.Since(callStart) != 0 {
						t.Fatalf("pending attempt slept/completed: done=%t elapsed=%s err=%v", done, time.Since(callStart), err)
					}
				case "foreign":
					if done || err == nil || !strings.Contains(err.Error(), "belongs to run") || len(client.deletes) != 0 {
						t.Fatalf("foreign object deleted: %v", err)
					}
				case "uid-conflict":
					if done || !apierrors.IsConflict(err) {
						t.Fatalf("UID conflict completed: %v", err)
					}
				default:
					if done || !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("request budget/cancellation lost: %t %v", done, err)
					}
				}
				if mode == "deleting" && len(client.deletes) != 0 {
					t.Fatal("already-deleting UID deleted again")
				}
			})
		})
	}
}

func TestWorkflowCreateCleanupSlowForegroundDeletionDefersPastWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		boundary := time.Now().Add(60 * time.Second)
		wf := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "wf", UID: "owned-uid", Annotations: map[string]string{runIDAnnotation: "run"}}}
		client := &cleanupWorkflowClient{}
		client.stored = wf
		client.deleteHook = func(context.Context, metav1.DeleteOptions) error {
			stamp := metav1.Now()
			client.stored.DeletionTimestamp = &stamp
			return nil
		}
		controller := &Controller{workflows: client}
		for _, advance := range []time.Duration{0, 60 * time.Second, 60 * time.Second} {
			time.Sleep(advance)
			done, err := controller.SettleCreate(t.Context(), "wf", "run", boundary)
			if done || err != nil {
				t.Fatalf("slow foreground deletion became fatal/done: %t %v", done, err)
			}
		}
		if len(client.deletes) != 1 {
			t.Fatal("deletion was reissued while terminating")
		}
		client.stored = nil
		done, err := controller.SettleCreate(t.Context(), "wf", "run", boundary)
		if !done || err != nil {
			t.Fatalf("recovered absence not settled: %t %v", done, err)
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
