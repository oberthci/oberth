package argojob

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/oberthci/oberth/pkg/periapsis"
)

var workflowTestResource = schema.GroupResource{Group: "argoproj.io", Resource: "workflows"}

type createRetryWorkflowClient struct {
	posts    []*wfv1.Workflow
	stored   *wfv1.Workflow
	getHook  func(context.Context, string) (*wfv1.Workflow, error)
	postHook func(context.Context, *wfv1.Workflow) (*wfv1.Workflow, error)
}

func (c *createRetryWorkflowClient) Get(ctx context.Context, name string, _ metav1.GetOptions) (*wfv1.Workflow, error) {
	if c.getHook != nil {
		return c.getHook(ctx, name)
	}
	if c.stored == nil {
		return nil, apierrors.NewNotFound(workflowTestResource, name)
	}
	return c.stored.DeepCopy(), nil
}
func (c *createRetryWorkflowClient) Create(ctx context.Context, wf *wfv1.Workflow, _ metav1.CreateOptions) (*wfv1.Workflow, error) {
	c.posts = append(c.posts, wf.DeepCopy())
	if c.postHook != nil {
		return c.postHook(ctx, wf)
	}
	c.stored = wf.DeepCopy()
	c.stored.UID = "persisted-workflow"
	return c.stored.DeepCopy(), nil
}
func (*createRetryWorkflowClient) Delete(context.Context, string, metav1.DeleteOptions) error {
	return errors.New("unexpected Workflow deletion")
}

func TestWorkflowCreateRetryReconcilesWithoutReseeding(t *testing.T) {
	for _, mode := range []string{"committed", "already-exists", "absent", "late-commit", "foreign-run", "foreign-spec", "exhausted"} {
		t.Run(mode, func(t *testing.T) {
			config := seedTestConfig()
			kube := fake.NewClientset()
			runningSeedPods(kube)
			client := &createRetryWorkflowClient{}
			controller, err := NewController(client, kube, config)
			if err != nil {
				t.Fatal(err)
			}
			var seeds []seedExecCall
			controller.WithSourceSeeder(NewSourceSeeder(kube, recordingExec(&seeds), config))
			request := testRequest(periapsis.TriggerCI, greedyDocument)
			request.SourceDir = writeCheckout(t)
			seedCount, reconciliationReads := 0, 0
			var pending *wfv1.Workflow
			client.postHook = func(_ context.Context, wf *wfv1.Workflow) (*wfv1.Workflow, error) {
				if len(client.posts) == 1 {
					seedCount = len(seeds)
				}
				pending = wf.DeepCopy()
				pending.UID = "persisted-workflow"
				pending.CreationTimestamp = metav1.NewTime(time.Unix(1234, 0))
				switch mode {
				case "committed", "already-exists", "foreign-run", "foreign-spec":
					client.stored = pending
					if mode == "foreign-run" {
						client.stored.Annotations[runIDAnnotation] = "some-other-run"
					}
					if mode == "foreign-spec" {
						client.stored.Annotations[identityAnnotation] = "some-other-source"
					}
				case "absent":
					if len(client.posts) == 2 {
						client.stored = pending
						return pending.DeepCopy(), nil
					}
				}
				if mode == "already-exists" {
					return nil, apierrors.NewAlreadyExists(workflowTestResource, wf.Name)
				}
				// Client mutation must not change the next POST's immutable spec.
				wf.Spec.ActiveDeadlineSeconds = nil
				return nil, apierrors.NewServerTimeout(workflowTestResource, "post", 0)
			}
			if mode == "late-commit" {
				client.getHook = func(_ context.Context, name string) (*wfv1.Workflow, error) {
					if len(client.posts) > 0 {
						reconciliationReads++
						if reconciliationReads == 2 {
							client.stored = pending
							return pending.DeepCopy(), nil
						}
					}
					return nil, apierrors.NewNotFound(workflowTestResource, name)
				}
			}
			synctest.Test(t, func(t *testing.T) {
				name, err := controller.Create(t.Context(), request)
				wantPosts := 1
				if mode == "absent" {
					wantPosts = 2
				}
				if mode == "exhausted" {
					wantPosts = workflowCreateAttempts
					var infra interface{ InfrastructureFailure() bool }
					if name != "" || !errors.As(err, &infra) || !infra.InfrastructureFailure() {
						t.Fatalf("exhaustion is not infrastructure: name=%s err=%v", name, err)
					}
				} else if strings.HasPrefix(mode, "foreign-") {
					if name != "" || err == nil || (!strings.Contains(err.Error(), "belongs to run") && !strings.Contains(err.Error(), "different spec")) {
						t.Fatalf("foreign Workflow accepted: name=%s err=%v", name, err)
					}
				} else if err != nil || name != request.Name || client.stored.UID != "persisted-workflow" || client.stored.CreationTimestamp.Unix() != 1234 {
					t.Fatalf("did not adopt/retry safely: name=%s err=%v", name, err)
				}
				if len(client.posts) != wantPosts || seedCount == 0 || len(seeds) != seedCount {
					t.Fatalf("duplicate create/seed: posts=%d want=%d seeds=%d initial=%d", len(client.posts), wantPosts, len(seeds), seedCount)
				}
				for _, post := range client.posts {
					if !reflect.DeepEqual(post, client.posts[0]) || post.Name != request.Name {
						t.Fatal("retry changed deterministic name, identity, source volume or deadline")
					}
				}
				claim, claimErr := kube.CoreV1().PersistentVolumeClaims(config.Namespace).Get(t.Context(), sourceClaimName(request.Name), metav1.GetOptions{})
				if claimErr != nil {
					t.Fatalf("uncertain/accepted create deleted source storage: %v", claimErr)
				}
				owned := len(claim.OwnerReferences) == 1 && claim.OwnerReferences[0].UID == "persisted-workflow"
				if (err == nil) != owned {
					t.Fatalf("claim adopted for wrong outcome: err=%v refs=%v", err, claim.OwnerReferences)
				}
			})
		})
	}
}

func TestWorkflowCreateRetryErrorClassesAndUncertainGet(t *testing.T) {
	for _, cause := range []error{
		apierrors.NewTimeoutError("private-api-response", 0),
		apierrors.NewServerTimeout(workflowTestResource, "post", 0),
		apierrors.NewGenericServerResponse(504, "post", workflowTestResource, "wf", "private-api-response", 0, false),
		apierrors.NewTooManyRequests("private-api-response", 0),
	} {
		t.Run(fmt.Sprintf("%T/%s", cause, executorIdentityErrorClass(cause)), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := &createRetryWorkflowClient{}
				client.postHook = func(context.Context, *wfv1.Workflow) (*wfv1.Workflow, error) { return nil, cause }
				controller := &Controller{workflows: client}
				request := testRequest(periapsis.TriggerCI, greedyDocument)
				intended, err := Build(testConfig(), request)
				if err != nil {
					t.Fatal(err)
				}
				_, rejected, err := controller.createWorkflowWithRetry(t.Context(), request, intended)
				if len(client.posts) != 3 || rejected || err == nil || strings.Contains(err.Error(), "private-api-response") || !errors.Is(err, cause) {
					t.Fatalf("incorrect retry/classification: posts=%d rejected=%t err=%v", len(client.posts), rejected, err)
				}
			})
		})
	}
	t.Run("GET outage never permits another POST", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			client := &createRetryWorkflowClient{}
			client.postHook = func(context.Context, *wfv1.Workflow) (*wfv1.Workflow, error) {
				return nil, apierrors.NewTimeoutError("timeout", 0)
			}
			client.getHook = func(context.Context, string) (*wfv1.Workflow, error) {
				return nil, apierrors.NewServiceUnavailable("not readable")
			}
			request := testRequest(periapsis.TriggerCI, greedyDocument)
			intended, err := Build(testConfig(), request)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			_, rejected, err := (&Controller{workflows: client}).createWorkflowWithRetry(t.Context(), request, intended)
			if len(client.posts) != 1 || rejected || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != workflowCreateBudget {
				t.Fatalf("unknown GET permitted POST/escaped budget: posts=%d elapsed=%s err=%v", len(client.posts), time.Since(start), err)
			}
		})
	})
}

func TestWorkflowCreateRetryStopsOnPolicyAndCancellation(t *testing.T) {
	for _, denied := range []error{
		apierrors.NewForbidden(workflowTestResource, "wf", errors.New("denied")),
		apierrors.NewBadRequest("invalid workflow"),
	} {
		synctest.Test(t, func(t *testing.T) {
			client := &createRetryWorkflowClient{postHook: func(context.Context, *wfv1.Workflow) (*wfv1.Workflow, error) { return nil, denied }}
			request := testRequest(periapsis.TriggerCI, greedyDocument)
			intended, err := Build(testConfig(), request)
			if err != nil {
				t.Fatal(err)
			}
			_, rejected, err := (&Controller{workflows: client}).createWorkflowWithRetry(t.Context(), request, intended)
			if len(client.posts) != 1 || !rejected || !errors.Is(err, denied) {
				t.Fatalf("policy refusal retried: posts=%d rejected=%t err=%v", len(client.posts), rejected, err)
			}
		})
	}
	synctest.Test(t, func(t *testing.T) {
		client := &createRetryWorkflowClient{}
		ctx, cancel := context.WithCancel(t.Context())
		client.postHook = func(context.Context, *wfv1.Workflow) (*wfv1.Workflow, error) {
			cancel()
			return nil, context.DeadlineExceeded
		}
		request := testRequest(periapsis.TriggerCI, greedyDocument)
		intended, err := Build(testConfig(), request)
		if err != nil {
			t.Fatal(err)
		}
		_, rejected, err := (&Controller{workflows: client}).createWorkflowWithRetry(ctx, request, intended)
		if len(client.posts) != 1 || rejected || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled owner retried/released uncertain source: posts=%d rejected=%t err=%v", len(client.posts), rejected, err)
		}
	})
}

func TestWorkflowCreateRetryHardRefusalAfterTimeoutRetainsUncertainSource(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := &createRetryWorkflowClient{}
		denied := apierrors.NewForbidden(workflowTestResource, "wf", errors.New("denied"))
		client.postHook = func(context.Context, *wfv1.Workflow) (*wfv1.Workflow, error) {
			if len(client.posts) == 1 {
				return nil, apierrors.NewTimeoutError("timeout", 0)
			}
			return nil, denied
		}
		request := testRequest(periapsis.TriggerCI, greedyDocument)
		intended, err := Build(testConfig(), request)
		if err != nil {
			t.Fatal(err)
		}
		_, rejected, err := (&Controller{workflows: client}).createWorkflowWithRetry(t.Context(), request, intended)
		var failure *workflowCreateFailure
		if len(client.posts) != 2 || rejected || !errors.Is(err, denied) || !errors.As(err, &failure) || failure.InfrastructureFailure() {
			t.Fatalf("hard refusal retried or released earlier uncertain POST: posts=%d rejected=%t err=%v", len(client.posts), rejected, err)
		}
	})
}

func TestWorkflowCreateRetryBudgetBoundsPostAndRetryAfter(t *testing.T) {
	for _, mode := range []string{"blocked-post", "retry-after"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := &createRetryWorkflowClient{}
				client.postHook = func(ctx context.Context, _ *wfv1.Workflow) (*wfv1.Workflow, error) {
					if mode == "blocked-post" {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					return nil, apierrors.NewTooManyRequests("busy", 120)
				}
				request := testRequest(periapsis.TriggerCI, greedyDocument)
				intended, err := Build(testConfig(), request)
				if err != nil {
					t.Fatal(err)
				}
				start := time.Now()
				_, rejected, err := (&Controller{workflows: client}).createWorkflowWithRetry(t.Context(), request, intended)
				if len(client.posts) != 1 || rejected || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != workflowCreateBudget {
					t.Fatalf("budget escaped: posts=%d elapsed=%s err=%v", len(client.posts), time.Since(start), err)
				}
			})
		})
	}
}

func TestWorkflowCreateRetryRechecksControllerProfile(t *testing.T) {
	for _, adopt := range []bool{false, true} {
		t.Run(fmt.Sprintf("adopt=%t", adopt), func(t *testing.T) {
			config, request, _, _ := trustedKVMBuilt(t)
			request = prepareTrustedTestRequest(t, config, request)
			intended, err := Build(config, request)
			if err != nil {
				t.Fatal(err)
			}
			pod, cm := nonrootVerifierObjects()
			cm.ResourceVersion = "2"
			kube := fake.NewClientset(pod, cm)
			client := &createRetryWorkflowClient{}
			client.postHook = func(context.Context, *wfv1.Workflow) (*wfv1.Workflow, error) {
				if adopt {
					client.stored = intended.DeepCopy()
				}
				return nil, apierrors.NewTimeoutError("timeout", 0)
			}
			controller, err := NewController(client, kube, config)
			if err != nil {
				t.Fatal(err)
			}
			synctest.Test(t, func(t *testing.T) {
				_, rejected, err := controller.createWorkflowWithRetry(t.Context(), request, intended)
				if len(client.posts) != 1 || rejected || err == nil || !strings.Contains(err.Error(), "controller identity changed") || errors.Is(err, ErrTrustedKVMCreateRefused) {
					t.Fatalf("profile change permitted retry/adoption or released uncertain grant: posts=%d rejected=%t err=%v", len(client.posts), rejected, err)
				}
			})
		})
	}
}
