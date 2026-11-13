package argowatch

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	clocktesting "k8s.io/utils/clock/testing"
)

func TestHealthListFailureAndRecovery(t *testing.T) {
	for _, kind := range []resourceKind{workflowKind, podKind} {
		t.Run(resourceName(kind), func(t *testing.T) {
			argo, kube := fakeArgo(), kubefake.NewSimpleClientset()
			var failing atomic.Bool
			failing.Store(true)
			reactor := func(clienttesting.Action) (bool, runtime.Object, error) {
				if failing.Load() {
					return true, nil, errors.New("API unavailable")
				}
				return false, nil, nil
			}
			if kind == workflowKind {
				argo.PrependReactor("list", "workflows", reactor)
			} else {
				kube.PrependReactor("list", "pods", reactor)
			}
			h := newTestHub(t, kube, argo)
			runHub(t, h)
			eventually(t, func() bool {
				h.healthMu.Lock()
				defer h.healthMu.Unlock()
				return !h.attempts[kind].failureSince.IsZero()
			}, "list failure recorded")
			if h.Healthy() {
				t.Fatal("unsynced cache is healthy")
			}
			if since, unhealthy := h.UnhealthySince(); !unhealthy || since != h.created {
				t.Fatalf("startup since = %v, %v", since, unhealthy)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if err := h.WaitForSync(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("sync during outage = %v", err)
			}
			failing.Store(false)
			syncHub(t, h)
			if since, unhealthy := h.UnhealthySince(); unhealthy || !since.IsZero() {
				t.Fatalf("recovered since = %v, %v", since, unhealthy)
			}
		})
	}
}

func TestHealthWatchFailureGraceAndRecovery(t *testing.T) {
	for _, kind := range []resourceKind{workflowKind, podKind} {
		t.Run(resourceName(kind), func(t *testing.T) {
			argo, kube := fakeArgo(), kubefake.NewSimpleClientset()
			var failing atomic.Bool
			var failures atomic.Int32
			initialWatch := make(chan watch.Interface, 1)
			tracker := argo.Tracker()
			resource := "workflows"
			if kind == podKind {
				tracker, resource = kube.Tracker(), "pods"
			}
			reactor := func(action clienttesting.Action) (bool, watch.Interface, error) {
				if handled, result, err := rejectStreamingList(action); handled {
					return handled, result, err
				}
				if failing.Load() {
					failures.Add(1)
					return true, nil, errors.New("API watch unavailable")
				}
				stream, err := tracker.Watch(action.GetResource(), action.GetNamespace())
				select {
				case initialWatch <- stream:
				default:
				}
				return true, stream, err
			}
			listReactor := func(clienttesting.Action) (bool, runtime.Object, error) {
				if failing.Load() {
					return true, nil, errors.New("API list unavailable")
				}
				return false, nil, nil
			}
			if kind == workflowKind {
				argo.PrependWatchReactor(resource, reactor)
				argo.PrependReactor("list", resource, listReactor)
			} else {
				kube.PrependWatchReactor(resource, reactor)
				kube.PrependReactor("list", resource, listReactor)
			}
			h := newTestHub(t, kube, argo)
			clock := clocktesting.NewFakeClock(time.Now())
			h.now = clock.Now
			stop := runHub(t, h)
			syncHub(t, h)
			var stream watch.Interface
			select {
			case stream = <-initialWatch:
			case <-time.After(15 * time.Second):
				t.Fatal("no initial watch")
			}
			// Deliver an event before dropping the watch, avoiding the
			// reflector's special handling of an empty, very short watch.
			if kind == workflowKind {
				if _, err := argo.ArgoprojV1alpha1().Workflows(testNamespace).Create(context.Background(), workflowObject("ping"), metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
				eventually(t, func() bool { _, ok := h.Workflow("ping"); return ok }, "Workflow watch event")
			} else {
				if _, err := kube.CoreV1().Pods(testNamespace).Create(context.Background(), podObject("ping", "ping"), metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
				eventually(t, func() bool { _, ok := h.Pod("ping"); return ok }, "Pod watch event")
			}
			clock.Step(time.Second)
			failureStart := clock.Now()
			failing.Store(true)
			stream.Stop() // A dropped connection forces the reflector to rewatch.
			eventually(t, func() bool { return failures.Load() > 0 }, "watch failure recorded")
			eventually(t, func() bool {
				h.healthMu.Lock()
				defer h.healthMu.Unlock()
				return !h.attempts[kind].failureSince.IsZero()
			}, "watch health failure recorded")
			if !h.Healthy() {
				t.Fatal("no grace after the first failure")
			}
			clock.Step(5*time.Second + time.Nanosecond)
			if h.Healthy() {
				t.Fatal("outage beyond grace remains healthy")
			}
			if since, unhealthy := h.UnhealthySince(); !unhealthy || !since.Equal(failureStart) {
				t.Fatalf("outage since = %v, %v; want %v", since, unhealthy, failureStart)
			}
			other := workflowKind
			if kind == workflowKind {
				other = podKind
			}
			h.recordAttempt(other, nil)
			if h.Healthy() {
				t.Fatal("other kind's success concealed outage")
			}
			clock.Step(3 * time.Minute)
			if since, unhealthy := h.UnhealthySince(); !unhealthy || !since.Equal(failureStart) {
				t.Fatal("prolonged outage lost its timestamp")
			}
			failing.Store(false)
			eventually(t, h.Healthy, "health recovery after successful list/watch")
			if since, unhealthy := h.UnhealthySince(); unhealthy || !since.IsZero() {
				t.Fatal("recovery retained outage")
			}
			clock.Step(time.Hour)
			if !h.Healthy() {
				t.Fatal("quiet watch must not become unhealthy")
			}
			stop()
			eventually(t, func() bool { return !h.Healthy() }, "shutdown health")
		})
	}
}

func resourceName(kind resourceKind) string {
	if kind == workflowKind {
		return "Workflow"
	}
	return "Pod"
}

func TestWatchErrorLoggingOmitsRawErrors(t *testing.T) {
	h := newTestHub(t, kubefake.NewSimpleClientset(), fakeArgo())
	var output bytes.Buffer
	h.logger = slog.New(slog.NewTextHandler(&output, nil))
	err := apierrors.NewForbidden(schema.GroupResource{Group: "argoproj.io", Resource: "workflows"}, "private-object", errors.New("private-contents"))
	h.watchErrorHandler(workflowKind)(context.Background(), nil, err)
	log := output.String()
	if !strings.Contains(log, "kind=Workflow") || !strings.Contains(log, "error_class=forbidden") || strings.Contains(log, "private-") {
		t.Fatalf("unsafe or incomplete error log: %s", log)
	}
}

func TestNewRequiresNamespaceAndClients(t *testing.T) {
	if _, err := New(kubefake.NewSimpleClientset(), fakeArgo(), "", nil); err == nil {
		t.Fatal("empty namespace accepted")
	}
	if _, err := New(nil, fakeArgo(), testNamespace, nil); err == nil {
		t.Fatal("nil Kubernetes client accepted")
	}
	if _, err := New(kubefake.NewSimpleClientset(), nil, testNamespace, nil); err == nil {
		t.Fatal("nil Argo client accepted")
	}
	if _, err := New(kubefake.NewSimpleClientset(), fakeArgo(), testNamespace, nil); err != nil {
		t.Fatal(err)
	}
}

func TestOverlappingOutagesKeepOriginalUnhealthySince(t *testing.T) {
	h := newTestHub(t, kubefake.NewSimpleClientset(), fakeArgo())
	clock := clocktesting.NewFakeClock(time.Now())
	h.now = clock.Now
	runHub(t, h)
	syncHub(t, h)
	clock.Step(time.Minute)
	firstFailure := clock.Now()
	h.recordAttempt(workflowKind, errors.New("Workflow API unavailable"))
	clock.Step(time.Minute)
	h.recordAttempt(podKind, errors.New("Pod API unavailable"))
	clock.Step(time.Minute)
	h.recordAttempt(workflowKind, nil)
	if since, unhealthy := h.UnhealthySince(); !unhealthy || !since.Equal(firstFailure) {
		t.Fatalf("overlap lost original outage time: %v, %v", since, unhealthy)
	}
	h.recordAttempt(podKind, nil)
	if !h.Healthy() {
		t.Fatal("both kinds recovered but hub is unhealthy")
	}
	clock.Step(time.Minute)
	secondFailure := clock.Now()
	h.recordAttempt(podKind, errors.New("Pod API unavailable again"))
	clock.Step(healthGrace + time.Nanosecond)
	if since, unhealthy := h.UnhealthySince(); !unhealthy || !since.Equal(secondFailure) {
		t.Fatalf("new outage retained old timestamp: %v, %v", since, unhealthy)
	}
}

func TestHealthGraceAfterQuietWatch(t *testing.T) {
	for _, kind := range []resourceKind{workflowKind, podKind} {
		t.Run(resourceName(kind), func(t *testing.T) {
			h := newTestHub(t, kubefake.NewSimpleClientset(), fakeArgo())
			clock := clocktesting.NewFakeClock(time.Now())
			h.now = clock.Now
			runHub(t, h)
			syncHub(t, h)
			clock.Step(time.Hour)
			failureStart := clock.Now()
			h.recordAttempt(kind, errors.New("API unavailable after quiet watch"))
			if !h.Healthy() {
				t.Fatal("quiet watch failure skipped the grace period")
			}
			if since, unhealthy := h.UnhealthySince(); unhealthy || !since.IsZero() {
				t.Fatal("grace reported an outage")
			}
			clock.Step(healthGrace)
			if !h.Healthy() {
				t.Fatal("grace ended before more than five seconds elapsed")
			}
			clock.Step(time.Nanosecond)
			if h.Healthy() {
				t.Fatal("failure beyond grace remained healthy")
			}
			if since, unhealthy := h.UnhealthySince(); !unhealthy || !since.Equal(failureStart) {
				t.Fatalf("outage timestamp = %v, %v; want %v", since, unhealthy, failureStart)
			}
			h.recordAttempt(kind, nil)
			if !h.Healthy() {
				t.Fatal("successful attempt did not recover health")
			}
		})
	}
}
