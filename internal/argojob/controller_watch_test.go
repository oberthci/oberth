package argojob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

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

func TestWatchWaitFailedConfirmationsRetryWithoutWatchEvents(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				final := watchWaitWorkflow(wfv1.WorkflowSucceeded)
				hub := newControlledWaitHub(final)
				if missing {
					hub = newControlledWaitHub(nil)
				}
				client := &countedWaitWorkflowClient{err: apierrors.NewServiceUnavailable("one failed read")}
				c := (&Controller{workflows: client, config: Config{Namespace: final.Namespace, MaxRunLogBytes: 4096}}).WithWatchHub(hub)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				result := startControlledWait(ctx, c, final)
				synctest.Wait()
				assertStillWaiting(t, result)
				if client.reads() != 1 {
					t.Fatal(client.reads())
				}
				var readErr error
				if missing {
					readErr = apierrors.NewNotFound(schema.GroupResource{Group: "argoproj.io", Resource: "workflows"}, final.Name)
				}
				client.set(final, readErr)
				time.Sleep(15 * time.Second)
				synctest.Wait()
				got := <-result
				if client.reads() != 2 || hub.subscriptions() != 0 || (missing && got.err == nil) || (!missing && (got.err != nil || !got.completion.Succeeded)) {
					t.Fatal(got, client.reads(), hub.subscriptions())
				}
			})
		})
	}
}

func TestWatchWaitOutageTimeDoesNotExpireIdentityClocks(t *testing.T) {
	for _, retried := range []bool{false, true} {
		t.Run(fmt.Sprint(retried), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, wf, pod, requests := executorRetryFixture(t)
				wf.Status.Phase = wfv1.WorkflowRunning
				pod.Status.InitContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(time.Now())
				if retried {
					pod.Annotations = map[string]string{executorIdentityRetryAnnotation: time.Now().Format(time.RFC3339Nano)}
				}
				hub := newControlledWaitHub(wf)
				hub.setPods(pod)
				final := wf.DeepCopy()
				final.Status.Phase = wfv1.WorkflowSucceeded
				client := &countedWaitWorkflowClient{workflow: final}
				c.workflows = client
				c.WithWatchHub(hub)
				var unavailable atomic.Bool
				unavailable.Store(true)
				var reads atomic.Int64
				c.kube.(*fake.Clientset).PrependReactor("get", "configmaps", func(ktesting.Action) (bool, runtime.Object, error) {
					reads.Add(1)
					if unavailable.Load() {
						return true, nil, apierrors.NewServiceUnavailable("temporary trust read")
					}
					return false, nil, nil
				})
				c.seeder.exec = func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
					return nil
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				result := startControlledWait(ctx, c, wf)
				synctest.Wait()
				if reads.Load() != 1 || *requests != 0 {
					t.Fatal(reads.Load(), *requests)
				}
				hub.setHealthy(false)
				time.Sleep(3 * time.Minute)
				synctest.Wait()
				assertStillWaiting(t, result)
				if reads.Load() != 1 {
					t.Fatal("outage performed identity reads", reads.Load())
				}
				hub.setHealthy(true)
				synctest.Wait()
				assertStillWaiting(t, result)
				if reads.Load() != 2 {
					t.Fatal("no first recovery attempt", reads.Load())
				}
				time.Sleep(time.Minute)
				synctest.Wait()
				assertStillWaiting(t, result)
				unavailable.Store(false)
				hub.setPods(pod)
				synctest.Wait()
				if *requests != 1 {
					t.Fatal("recovery did not deliver identity", *requests)
				}
				hub.setWorkflow(final)
				synctest.Wait()
				if got := <-result; got.err != nil || !got.completion.Succeeded || hub.subscriptions() != 0 {
					t.Fatal(got)
				}
			})
		})
	}
}

func TestWatchWaitCompletionClassifiesLivePodNotStaleCache(t *testing.T) {
	wf, _, pod, _ := diagnosticFixture()
	stale := pod.DeepCopy()
	stale.Status.ContainerStatuses = nil
	hub := newControlledWaitHub(wf)
	hub.setPods(stale)
	hub.setHealthy(false)
	kube := fake.NewClientset(pod)
	client := &countedWaitWorkflowClient{workflow: wf}
	c := (&Controller{workflows: client, kube: kube, config: Config{Namespace: wf.Namespace, MaxRunLogBytes: 4096}}).WithWatchHub(hub)
	completion, err := c.Wait(t.Context(), wf.Name, wf.Annotations[runIDAnnotation], io.Discard)
	if err != nil || len(completion.Steps) != 1 || completion.Steps[0].Termination == nil || !completion.Steps[0].Termination.Available || completion.Steps[0].Termination.Container.Reason != "OOMKilled" {
		t.Fatal(completion, err)
	}
	lists := 0
	for _, action := range kube.Actions() {
		if action.GetVerb() == "list" && action.GetResource().Resource == "pods" {
			lists++
		}
	}
	if lists != 1 || client.reads() != 1 {
		t.Fatal("completion snapshot/settle count", lists, client.reads())
	}
}

func TestWatchWaitIdleWithoutWaiterDoesNotRevalidateCapability(t *testing.T) {
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
}

func TestWatchWaitIdleOutageCompletionAndCleanup(t *testing.T) {
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
}

func TestWatchWaitPodEventDeliversToken(t *testing.T) {
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
}

func TestWatchWaitNodeBootReadsAreMemoizedAndNotIdlePolling(t *testing.T) {
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
}

func TestWatchWaitFailedSettleWaitsForRecovery(t *testing.T) {
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
}

func TestWatchWaitTerminalWithUnavailablePodCache(t *testing.T) {
	final := watchWaitWorkflow(wfv1.WorkflowSucceeded)
	hub := newControlledWaitHub(final)
	hub.setHealthy(false)
	client := &countedWaitWorkflowClient{workflow: final}
	c := (&Controller{workflows: client, config: Config{Namespace: "pipeline", MaxRunLogBytes: 4096}}).WithWatchHub(hub)
	completion, err := c.Wait(t.Context(), final.Name, "run", io.Discard)
	if err != nil || !completion.Succeeded || client.reads() != 1 || hub.subscriptions() != 0 {
		t.Fatal(completion, err, client.reads())
	}
}

func TestWatchWaitCacheMissConfirmsDeletion(t *testing.T) {
	wf := watchWaitWorkflow(wfv1.WorkflowRunning)
	hub := newControlledWaitHub(nil)
	client := &countedWaitWorkflowClient{err: apierrors.NewNotFound(schema.GroupResource{Group: "argoproj.io", Resource: "workflows"}, wf.Name)}
	c := (&Controller{workflows: client, config: Config{Namespace: "pipeline", MaxRunLogBytes: 4096}}).WithWatchHub(hub)
	_, err := c.Wait(t.Context(), wf.Name, "run", io.Discard)
	if err == nil || client.reads() != 1 || hub.subscriptions() != 0 {
		t.Fatal(err, client.reads())
	}
}

func TestWatchWaitLiveNonterminalIsNotAVerdict(t *testing.T) {
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
		if client.reads() != 5 {
			t.Fatal("owed completion was not retried on each idle tick", client.reads())
		}
		client.set(cached, nil)
		time.Sleep(15 * time.Second)
		synctest.Wait()
		if got := <-result; got.err != nil || !got.completion.Succeeded {
			t.Fatal(got)
		}
	})
}

func TestWatchWaitCancellationRemovesSubscription(t *testing.T) {
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
}
