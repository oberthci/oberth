package argojob

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/oberthci/oberth/internal/model"
)

type controlledWaitHub struct {
	mu       sync.Mutex
	workflow *wfv1.Workflow
	pods     []*corev1.Pod
	healthy  bool
	since    time.Time
	subs     map[chan struct{}]struct{}
}

func newControlledWaitHub(wf *wfv1.Workflow) *controlledWaitHub {
	return &controlledWaitHub{workflow: wf, healthy: true, subs: map[chan struct{}]struct{}{}}
}
func (h *controlledWaitHub) Subscribe(string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() { once.Do(func() { h.mu.Lock(); delete(h.subs, ch); h.mu.Unlock() }) }
}
func (h *controlledWaitHub) Workflow(string) (*wfv1.Workflow, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.workflow, h.workflow != nil
}
func (h *controlledWaitHub) PodsOf(string) []*corev1.Pod {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pods
}
func (h *controlledWaitHub) Pods() []*corev1.Pod { return h.PodsOf("") }
func (h *controlledWaitHub) Healthy() bool       { h.mu.Lock(); defer h.mu.Unlock(); return h.healthy }
func (h *controlledWaitHub) Synced() bool        { return true }
func (h *controlledWaitHub) UnhealthySince() (time.Time, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.since, !h.healthy
}
func (h *controlledWaitHub) notifyLocked() {
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
func (h *controlledWaitHub) setWorkflow(wf *wfv1.Workflow) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.workflow = wf.DeepCopy()
	h.notifyLocked()
}
func (h *controlledWaitHub) setPods(pods ...*corev1.Pod) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pods = pods
	h.notifyLocked()
}
func (h *controlledWaitHub) setHealthy(healthy bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	recovered := !h.healthy && healthy
	h.healthy = healthy
	if !healthy {
		h.since = time.Now()
	}
	if recovered {
		h.notifyLocked()
	}
}

func (h *controlledWaitHub) subscriptions() int { h.mu.Lock(); defer h.mu.Unlock(); return len(h.subs) }

type countedWaitWorkflowClient struct {
	WorkflowClient
	mu       sync.Mutex
	workflow *wfv1.Workflow
	err      error
	gets     int
}

func (c *countedWaitWorkflowClient) Get(ctx context.Context, _ string, _ metav1.GetOptions) (*wfv1.Workflow, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets++
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if c.err != nil {
		return nil, c.err
	}
	return c.workflow.DeepCopy(), nil
}
func (c *countedWaitWorkflowClient) set(wf *wfv1.Workflow, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.workflow = wf
	c.err = err
}
func (c *countedWaitWorkflowClient) reads() int { c.mu.Lock(); defer c.mu.Unlock(); return c.gets }

type waitResult struct {
	completion Completion
	err        error
}

func startControlledWait(ctx context.Context, c *Controller, wf *wfv1.Workflow) <-chan waitResult {
	result := make(chan waitResult, 1)
	go func() {
		completion, err := c.Wait(ctx, wf.Name, wf.Annotations[runIDAnnotation], io.Discard)
		result <- waitResult{completion, err}
	}()
	return result
}
func watchWaitWorkflow(phase wfv1.WorkflowPhase) *wfv1.Workflow {
	return &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "watch-run", Namespace: "pipeline", UID: "workflow-uid", Annotations: map[string]string{runIDAnnotation: "run"}}, Status: wfv1.WorkflowStatus{Phase: phase}}
}
func assertStillWaiting(t *testing.T, result <-chan waitResult) {
	t.Helper()
	select {
	case got := <-result:
		t.Fatalf("observation ended without a native verdict: %+v", got)
	default:
	}
}

type watchNodeBootRecorder struct{}

func (watchNodeBootRecorder) ObserveNodeBoot(context.Context, model.NodeBootObservation, bool) (*model.NodeReboot, error) {
	return nil, nil
}

func TestWatchWait(t *testing.T) {
	t.Run("idle_without_waiter_does_not_revalidate_capability", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			wf := watchWaitWorkflow(wfv1.WorkflowRunning)
			wf.Annotations[capabilityAnnotation] = `{"repository_id":1}`
			hub := newControlledWaitHub(wf)
			client := &countedWaitWorkflowClient{}
			kube := fake.NewClientset()
			c := (&Controller{workflows: client, kube: kube, seeder: &SourceSeeder{}, config: Config{Namespace: wf.Namespace, MaxRunLogBytes: 4096}}).WithWatchHub(hub)
			ctx, cancel := context.WithCancel(t.Context())
			result := startControlledWait(ctx, c, wf)
			synctest.Wait()
			time.Sleep(time.Minute)
			synctest.Wait()
			assertStillWaiting(t, result)
			if client.reads() != 0 || len(kube.Actions()) != 0 {
				t.Fatal("idle observation revalidated delivery authority", client.reads(), kube.Actions())
			}
			cancel()
			synctest.Wait()
			if got := <-result; !errors.Is(got.err, context.Canceled) || hub.subscriptions() != 0 {
				t.Fatal(got, hub.subscriptions())
			}
		})
	})
	t.Run("idle_outage_completion_and_cleanup", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			running := watchWaitWorkflow(wfv1.WorkflowRunning)
			hub := newControlledWaitHub(running)
			final := running.DeepCopy()
			final.Status.Phase = wfv1.WorkflowFailed
			client := &countedWaitWorkflowClient{workflow: final}
			kube := fake.NewClientset()
			c := (&Controller{workflows: client, kube: kube, config: Config{Namespace: "pipeline", MaxRunLogBytes: 4096}}).WithWatchHub(hub)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := startControlledWait(ctx, c, running)
			synctest.Wait()
			if hub.subscriptions() != 1 {
				t.Fatal("missing subscription")
			}
			time.Sleep(time.Minute)
			synctest.Wait()
			if client.reads() != 0 || len(kube.Actions()) != 0 {
				t.Fatal("idle observation polled APIs", client.reads(), kube.Actions())
			}
			hub.setHealthy(false)
			synctest.Wait()
			time.Sleep(5 * time.Minute)
			synctest.Wait()
			assertStillWaiting(t, result)
			if client.reads() != 0 || len(kube.Actions()) != 0 {
				t.Fatal("outage performed API reads")
			}
			// The live failed phase overrides the cached succeeded phase.
			cached := running.DeepCopy()
			cached.Status.Phase = wfv1.WorkflowSucceeded
			hub.setWorkflow(cached)
			hub.setHealthy(true)
			synctest.Wait()
			got := <-result
			if got.err != nil || !got.completion.Terminal || got.completion.Succeeded || client.reads() != 1 {
				t.Fatal(got, client.reads())
			}
			if hub.subscriptions() != 0 {
				t.Fatal("subscription leaked after completion")
			}
			for _, action := range kube.Actions() {
				if action.GetVerb() == "delete" {
					t.Fatal("outage deleted native work")
				}
			}
		})
	})
	t.Run("pod_event_delivers_token", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c, wf, pod, requests := executorRetryFixture(t)
			wf.Status.Phase = wfv1.WorkflowRunning
			pending := pod.DeepCopy()
			pending.Status.InitContainerStatuses = nil
			hub := newControlledWaitHub(wf)
			hub.setPods(pending)
			final := wf.DeepCopy()
			final.Status.Phase = wfv1.WorkflowSucceeded
			client := &countedWaitWorkflowClient{workflow: final}
			c.workflows = client
			c.WithWatchHub(hub)
			delivered := make(chan struct{}, 1)
			c.seeder.exec = func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
				delivered <- struct{}{}
				return nil
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := startControlledWait(ctx, c, wf)
			synctest.Wait()
			if *requests != 0 {
				t.Fatal("issued before a running waiter event")
			}
			hub.setPods(pod.DeepCopy())
			synctest.Wait()
			select {
			case <-delivered:
			default:
				t.Fatal("Pod event did not deliver identity")
			}
			if *requests != 1 || client.reads() != 0 {
				t.Fatal(*requests, client.reads())
			}
			actions := len(c.kube.(*fake.Clientset).Actions())
			time.Sleep(30 * time.Second)
			synctest.Wait()
			if len(c.kube.(*fake.Clientset).Actions()) != actions || client.reads() != 0 {
				t.Fatal("idle token observation polled APIs")
			}
			hub.setWorkflow(final)
			synctest.Wait()
			if got := <-result; got.err != nil || !got.completion.Succeeded {
				t.Fatal(got)
			}
			if client.reads() != 1 || hub.subscriptions() != 0 {
				t.Fatal("settle/subscription count", client.reads(), hub.subscriptions())
			}
		})
	})
	t.Run("node_boot_reads_are_memoized_and_not_idle_polling", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			wf, node, pod, _ := diagnosticFixtureAt(time.Now().Add(-time.Minute))
			wf.Status.Phase = wfv1.WorkflowRunning
			node.Phase = wfv1.NodeRunning
			wf.Status.Nodes[node.ID] = node
			pod.Spec.NodeName = "worker"
			pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: node.StartedAt}}
			machine := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "node-uid"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: "boot"}}}
			kube := fake.NewClientset(machine)
			hub := newControlledWaitHub(wf)
			hub.setPods(pod)
			client := &countedWaitWorkflowClient{}
			c := (&Controller{workflows: client, kube: kube, config: Config{Namespace: wf.Namespace, MaxRunLogBytes: 4096}}).WithWatchHub(hub).WithNodeBootRecorder(watchNodeBootRecorder{})
			ctx, cancel := context.WithCancel(t.Context())
			result := startControlledWait(ctx, c, wf)
			synctest.Wait()
			for range 3 {
				hub.setWorkflow(wf)
				synctest.Wait()
			}
			time.Sleep(time.Minute)
			synctest.Wait()
			reads := 0
			for _, action := range kube.Actions() {
				if action.GetVerb() == "get" && action.GetResource().Resource == "nodes" {
					reads++
				} else {
					t.Fatal("unexpected wait observation API call", action)
				}
			}
			if reads != 1 || client.reads() != 0 {
				t.Fatal("node boot was polled", reads, client.reads())
			}
			cancel()
			synctest.Wait()
			<-result
		})
	})
	t.Run("failed_settle_waits_for_recovery", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			final := watchWaitWorkflow(wfv1.WorkflowSucceeded)
			hub := newControlledWaitHub(final)
			client := &countedWaitWorkflowClient{err: apierrors.NewServiceUnavailable("API unavailable")}
			c := (&Controller{workflows: client, config: Config{Namespace: "pipeline", MaxRunLogBytes: 4096}}).WithWatchHub(hub)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := startControlledWait(ctx, c, final)
			synctest.Wait()
			if client.reads() != 1 {
				t.Fatal(client.reads())
			}
			hub.setHealthy(false)
			time.Sleep(5 * time.Minute)
			synctest.Wait()
			assertStillWaiting(t, result)
			if client.reads() != 1 {
				t.Fatal("failed settle polled while unavailable", client.reads())
			}
			client.set(final, nil)
			hub.setHealthy(true)
			synctest.Wait()
			got := <-result
			if got.err != nil || !got.completion.Succeeded || client.reads() != 2 || hub.subscriptions() != 0 {
				t.Fatal(got, client.reads())
			}
		})
	})
	t.Run("terminal_with_unavailable_pod_cache", func(t *testing.T) {
		final := watchWaitWorkflow(wfv1.WorkflowSucceeded)
		hub := newControlledWaitHub(final)
		hub.setHealthy(false)
		client := &countedWaitWorkflowClient{workflow: final}
		c := (&Controller{workflows: client, config: Config{Namespace: "pipeline", MaxRunLogBytes: 4096}}).WithWatchHub(hub)
		completion, err := c.Wait(t.Context(), final.Name, "run", io.Discard)
		if err != nil || !completion.Succeeded || client.reads() != 1 || hub.subscriptions() != 0 {
			t.Fatal(completion, err, client.reads())
		}
	})
	t.Run("cache_miss_confirms_deletion", func(t *testing.T) {
		wf := watchWaitWorkflow(wfv1.WorkflowRunning)
		hub := newControlledWaitHub(nil)
		client := &countedWaitWorkflowClient{err: apierrors.NewNotFound(schema.GroupResource{Group: "argoproj.io", Resource: "workflows"}, wf.Name)}
		c := (&Controller{workflows: client, config: Config{Namespace: "pipeline", MaxRunLogBytes: 4096}}).WithWatchHub(hub)
		_, err := c.Wait(t.Context(), wf.Name, "run", io.Discard)
		if err == nil || client.reads() != 1 || hub.subscriptions() != 0 {
			t.Fatal(err, client.reads())
		}
	})
	t.Run("live_nonterminal_is_not_a_verdict", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			cached := watchWaitWorkflow(wfv1.WorkflowSucceeded)
			hub := newControlledWaitHub(cached)
			running := cached.DeepCopy()
			running.Status.Phase = wfv1.WorkflowRunning
			client := &countedWaitWorkflowClient{workflow: running}
			c := (&Controller{workflows: client, config: Config{Namespace: "pipeline", MaxRunLogBytes: 4096}}).WithWatchHub(hub)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := startControlledWait(ctx, c, cached)
			synctest.Wait()
			time.Sleep(time.Minute)
			synctest.Wait()
			assertStillWaiting(t, result)
			if client.reads() != 1 {
				t.Fatal("completion polled between events", client.reads())
			}
			client.set(cached, nil)
			hub.setWorkflow(cached)
			synctest.Wait()
			if got := <-result; got.err != nil || !got.completion.Succeeded {
				t.Fatal(got)
			}
		})
	})
	t.Run("cancellation_removes_subscription", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			wf := watchWaitWorkflow(wfv1.WorkflowRunning)
			hub := newControlledWaitHub(wf)
			c := (&Controller{workflows: &countedWaitWorkflowClient{}, config: Config{Namespace: "pipeline", MaxRunLogBytes: 4096}}).WithWatchHub(hub)
			ctx, cancel := context.WithCancel(t.Context())
			result := startControlledWait(ctx, c, wf)
			synctest.Wait()
			cancel()
			synctest.Wait()
			if got := <-result; !errors.Is(got.err, context.Canceled) {
				t.Fatal(got)
			}
			if hub.subscriptions() != 0 {
				t.Fatal("subscription leaked on cancellation")
			}
		})
	})
}
