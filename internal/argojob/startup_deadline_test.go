package argojob

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	wffake "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned/fake"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

func TestPendingWorkloadHasNoServerStartupDeadline(t *testing.T) {
	for _, scenario := range []string{"no pods", "identity holder", "foreign scheduled pod", "restart"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				config := testConfig()
				wf := schedulingWorkflow()
				wf.Namespace, wf.UID = config.Namespace, "workflow-uid"
				wf.Spec.ActiveDeadlineSeconds = ptr.To(int64(7200))
				begin := time.Now()
				wf.CreationTimestamp = metav1.NewTime(begin)
				if scenario == "restart" {
					wf.CreationTimestamp = metav1.NewTime(begin.Add(-time.Hour))
				}
				client := wffake.NewSimpleClientset(wf)
				kube := fake.NewClientset()
				kube.PrependReactor("create", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
					action.(ktesting.CreateAction).GetObject().(*corev1.Pod).UID = "holder-uid"
					return false, nil, nil
				})
				controller, err := NewController(client.ArgoprojV1alpha1().Workflows(config.Namespace), kube, config)
				if err != nil {
					t.Fatal(err)
				}
				controller.idleInterval = time.Minute
				if scenario == "identity holder" {
					if _, err := controller.executorIdentityPod(t.Context(), wf, config.ExecutorServiceAccount); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "foreign scheduled pod" {
					pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: wf.Namespace, Labels: map[string]string{"workflows.argoproj.io/workflow": wf.Name}}, Spec: corev1.PodSpec{NodeName: "worker"}}
					if _, err := kube.CoreV1().Pods(wf.Namespace).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
				defer cancel()
				digest, hashErr := adoptionSpecHash(wf)
				if hashErr != nil {
					t.Fatal(hashErr)
				}
				_, err = withWaitTestHub(t, controller).WaitResumed(ctx, wf.Name, "run", io.Discard, nil, 0, Adoption{UID: wf.UID, Identity: wf.Annotations[identityAnnotation], SpecSHA256: digest})
				want := 15 * time.Minute
				if !errors.Is(err, context.DeadlineExceeded) || time.Since(begin) != want {
					t.Fatalf("startup deadline: elapsed=%s want=%s, error=%v", time.Since(begin), want, err)
				}
				for _, action := range kube.Actions() {
					if action.GetVerb() == "patch" || action.GetVerb() == "update" {
						t.Fatal("identity holder gate was changed")
					}
				}
			})
		})
	}
}

func TestStartupDeadlineExecutorReadRetriesAreBoundedAcrossRestart(t *testing.T) {
	for _, age := range []time.Duration{0, time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				controller, wf, pod, _ := executorRetryFixture(t)
				begin := time.Now()
				wf.CreationTimestamp = metav1.NewTime(begin.Add(-2 * time.Hour))
				wf.Status.StartedAt = metav1.NewTime(begin)
				wf.Spec.ActiveDeadlineSeconds = ptr.To(int64(7200))
				pod.CreationTimestamp = metav1.NewTime(begin.Add(-age))
				pod.Spec.NodeName = "worker"
				if _, err := controller.kube.CoreV1().Pods(wf.Namespace).Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
				controller.kube.(*fake.Clientset).PrependReactor("get", "configmaps", func(ktesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewServiceUnavailable("busy")
				})
				client := wffake.NewSimpleClientset(wf)
				controller.workflows = client.ArgoprojV1alpha1().Workflows(wf.Namespace)
				controller.idleInterval = time.Minute
				ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
				defer cancel()
				_, err := withWaitTestHub(t, controller).Wait(ctx, wf.Name, "identity-run", io.Discard)
				want := 2 * time.Minute
				if err == nil || !strings.Contains(err.Error(), "Kubernetes executor identity observation unavailable") || time.Since(begin) != want {
					t.Fatalf("unbounded identity retry: elapsed=%s error=%v", time.Since(begin), err)
				}
			})
		})
	}
}

func TestStartupDeadlineScheduledWorkSurvives(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		config := testConfig()
		wf := schedulingWorkflow()
		wf.Namespace = config.Namespace
		begin := time.Now()
		wf.CreationTimestamp = metav1.NewTime(begin)
		wf.Spec.ActiveDeadlineSeconds = ptr.To(int64(7200))
		client := wffake.NewSimpleClientset(wf)
		client.PrependReactor("get", "workflows", func(ktesting.Action) (bool, runtime.Object, error) {
			observed := wf.DeepCopy()
			if time.Since(begin) >= 5*time.Minute {
				observed.Status.Nodes["real"] = wfv1.NodeStatus{ID: "real", Type: wfv1.NodeTypePod, HostNodeName: "worker", Phase: wfv1.NodeRunning}
			}
			if time.Since(begin) >= 12*time.Minute {
				observed.Status.Phase = wfv1.WorkflowSucceeded
			}
			return true, observed, nil
		})
		controller, err := NewController(client.ArgoprojV1alpha1().Workflows(config.Namespace), fake.NewClientset(), config)
		if err != nil {
			t.Fatal(err)
		}
		controller.idleInterval = time.Minute
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
		defer cancel()
		result, err := withWaitTestHub(t, controller).Wait(ctx, wf.Name, "run", io.Discard)
		if err != nil || !result.Succeeded {
			t.Fatalf("real progress failed: %+v %v", result, err)
		}
	})
}

func TestPodObservationOutageDoesNotExpireRun(t *testing.T) {
	for _, outage := range []time.Duration{time.Minute, 3 * time.Minute} {
		t.Run(outage.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				config := testConfig()
				wf := schedulingWorkflow()
				wf.Namespace = config.Namespace
				begin := time.Now()
				wf.CreationTimestamp = metav1.NewTime(begin)
				client := wffake.NewSimpleClientset(wf)
				client.PrependReactor("get", "workflows", func(ktesting.Action) (bool, runtime.Object, error) {
					observed := wf.DeepCopy()
					if time.Since(begin) >= 3*time.Minute {
						observed.Status.Phase = wfv1.WorkflowSucceeded
					}
					return true, observed, nil
				})
				kube := fake.NewClientset()
				kube.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
					if time.Since(begin) < outage {
						return true, nil, apierrors.NewServiceUnavailable("private response")
					}
					return false, nil, nil
				})
				controller, err := NewController(client.ArgoprojV1alpha1().Workflows(config.Namespace), kube, config)
				if err != nil {
					t.Fatal(err)
				}
				controller.idleInterval = time.Minute
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
				defer cancel()
				result, err := withWaitTestHub(t, controller).Wait(ctx, wf.Name, "run", io.Discard)
				if err != nil || !result.Succeeded || time.Since(begin) != 3*time.Minute {
					t.Fatalf("observation outage replaced the native verdict: outage=%s elapsed=%s result=%+v err=%v", outage, time.Since(begin), result, err)
				}

			})
		})
	}
}

func TestUnstartedWaiterHasNoServerStartupDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		controller, wf, pod, _ := executorRetryFixture(t)
		begin := time.Now()
		wf.CreationTimestamp = metav1.NewTime(begin.Add(-30 * time.Minute))
		wf.Spec.ActiveDeadlineSeconds = ptr.To(int64(7200))
		wf.Status.Nodes = wfv1.Nodes{"old": {ID: "old", Type: wfv1.NodeTypePod, HostNodeName: "worker", Phase: wfv1.NodeSucceeded}}
		pod.CreationTimestamp = metav1.NewTime(begin)
		pod.Spec.NodeName = "worker"
		pod.Status.InitContainerStatuses = nil // no waiter-running event is ever delivered
		if _, err := controller.kube.CoreV1().Pods(wf.Namespace).Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		client := wffake.NewSimpleClientset(wf)
		controller.workflows = client.ArgoprojV1alpha1().Workflows(wf.Namespace)
		controller.idleInterval = time.Minute
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
		defer cancel()
		_, err := withWaitTestHub(t, controller).Wait(ctx, wf.Name, "identity-run", io.Discard)
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(begin) != 15*time.Minute {
			t.Fatalf("unstarted waiter was terminated by a server startup deadline: %s %v", time.Since(begin), err)
		}
	})
}

// A slow Kubernetes call must be bounded even before a waiter or Workflow
// start event exists. Fake reactors do not receive contexts; wrap the client.
type blockedStartupWorkflow struct {
	WorkflowClient
	reads    int
	deadline time.Time
}

func (w *blockedStartupWorkflow) Get(ctx context.Context, _ string, _ metav1.GetOptions) (*wfv1.Workflow, error) {
	w.reads++
	if w.reads == 1 {
		w.deadline, _ = ctx.Deadline()
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestStartupDeadlineFirstWorkflowReadIsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := &blockedStartupWorkflow{}
		controller, err := NewController(client, fake.NewClientset(), testConfig())
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 61*time.Second)
		defer cancel()
		begin := time.Now()
		hub := newControlledWaitHub(nil)
		_, err = controller.WithWatchHub(hub).Wait(ctx, "missing", "run", io.Discard)
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(begin) != 61*time.Second || client.reads != 4 || client.deadline.Sub(begin) != executorIdentityAttemptTimeout || hub.subscriptions() != 0 {
			t.Fatalf("first read/outage: elapsed=%s calls=%d read deadline=%s subscriptions=%d err=%v", time.Since(begin), client.reads, client.deadline.Sub(begin), hub.subscriptions(), err)
		}
	})
}
