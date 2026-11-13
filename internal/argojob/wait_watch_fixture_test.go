package argojob

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Legacy scripted clients advance on reads. This fixture feeds those scripts
// into snapshots and wake-ups; production has no polling observation path.
type scriptedWaitHub struct {
	controller *Controller
	step       time.Duration
	ctx        context.Context
	mu         sync.Mutex
	workflow   *wfv1.Workflow
	pods       []*corev1.Pod
	healthy    bool
	subs       map[chan struct{}]struct{}
	stop       context.CancelFunc
	done       chan struct{}
}

var waitFixtureMu sync.Mutex

func withWaitTestHub(t *testing.T, c *Controller, actual ...ObservationHub) *Controller {
	t.Helper()
	waitFixtureMu.Lock()
	defer waitFixtureMu.Unlock()
	if len(actual) > 0 {
		c.WithWatchHub(actual[0])
		return c
	}
	if c.hub != nil {
		return c
	}
	step := c.idleInterval
	if step <= 0 || step == waitIdleInterval {
		step = time.Millisecond
	}
	c.WithWatchHub(&scriptedWaitHub{controller: c, step: step, ctx: t.Context(), subs: map[chan struct{}]struct{}{}})
	return c
}
func (h *scriptedWaitHub) Subscribe(name string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	if h.stop == nil {
		h.healthy = false
		ctx, cancel := context.WithCancel(h.ctx)
		h.stop = cancel
		h.done = make(chan struct{})
		done := h.done
		go func() { defer close(done); h.feed(ctx, name) }()
	}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			var done chan struct{}
			if len(h.subs) == 0 && h.stop != nil {
				h.stop()
				h.stop = nil
				done = h.done
			}
			h.mu.Unlock()
			if done != nil {
				<-done
			}
		})
	}

}
func (h *scriptedWaitHub) feed(ctx context.Context, name string) {
	ticker := time.NewTicker(h.step)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		wf, err := h.controller.workflows.Get(ctx, name, metav1.GetOptions{})
		workflowReadOK := err == nil && wf != nil
		var pods []*corev1.Pod
		if workflowReadOK && h.controller.kube != nil {
			// Model the informer's complete paged snapshot, including a failed page.
			reader := &Controller{kube: h.controller.kube}
			snapshot, reason := reader.timingPodSnapshot(ctx, wf)
			if reason != "" {
				err = errors.New(reason)
			} else {
				for _, pod := range snapshot {
					pods = append(pods, pod.DeepCopy())
				}
				sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
			}
		}

		h.mu.Lock()
		if ctx.Err() != nil {
			h.mu.Unlock()
			return
		}
		if workflowReadOK {
			h.workflow = wf.DeepCopy()
			if err == nil {
				h.pods = pods
			}
		}
		h.healthy = err == nil && wf != nil
		for sub := range h.subs {
			select {
			case sub <- struct{}{}:
			default:
			}
		}
		h.mu.Unlock()
		if workflowReadOK && wf.Status.Phase.Completed() {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (h *scriptedWaitHub) Workflow(string) (*wfv1.Workflow, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.workflow, h.workflow != nil
}
func (h *scriptedWaitHub) PodsOf(string) []*corev1.Pod {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pods
}
func (h *scriptedWaitHub) Pods() []*corev1.Pod               { return h.PodsOf("") }
func (h *scriptedWaitHub) Synced() bool                      { return true }
func (h *scriptedWaitHub) Healthy() bool                     { h.mu.Lock(); defer h.mu.Unlock(); return h.healthy }
func (h *scriptedWaitHub) UnhealthySince() (time.Time, bool) { return time.Time{}, !h.Healthy() }
