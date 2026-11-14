package argowatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	versioned "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned"
	argoinformers "github.com/argoproj/argo-workflows/v4/pkg/client/informers/externalversions"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

const (
	workflowSelector = "oberth.ci/engine=argo"
	podSelector      = "oberth.ci/trigger"
	workflowLabel    = "workflows.argoproj.io/workflow"
	byWorkflow       = "byWorkflow"
	resyncPeriod     = 10 * time.Minute
)

// Hub holds read-only caches and coalescing notifications for one namespace.
// Construct it with New; the zero value is not usable.
type Hub struct {
	namespace string
	logger    *slog.Logger
	workflows cache.SharedIndexInformer
	pods      cache.SharedIndexInformer
	argo      argoinformers.SharedInformerFactory
	kube      informers.SharedInformerFactory
	runOnce   sync.Once

	subMu sync.Mutex
	subs  map[string]map[chan struct{}]struct{}

	healthMu       sync.Mutex
	attempts       [2]attemptHealth
	created        time.Time
	stopped        time.Time
	unhealthySince time.Time
	now            func() time.Time
}

// New prepares both informers without starting any goroutines or API requests.
func New(kube kubernetes.Interface, argo versioned.Interface, namespace string, logger *slog.Logger) (*Hub, error) {
	if kube == nil || argo == nil || namespace == "" {
		return nil, errors.New("argowatch: Kubernetes and Argo clients and a pipeline namespace are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	h := &Hub{
		namespace: namespace, logger: logger, now: time.Now, created: time.Now(),
		subs: make(map[string]map[chan struct{}]struct{}),
	}
	h.argo = argoinformers.NewSharedInformerFactoryWithOptions(argo, resyncPeriod,
		argoinformers.WithNamespace(namespace), argoinformers.WithTweakListOptions(selectWorkflows))
	h.kube = informers.NewSharedInformerFactoryWithOptions(kube, resyncPeriod,
		informers.WithNamespace(namespace), informers.WithTweakListOptions(selectPods))
	h.workflows = h.argo.InformerFor(&wfv1.Workflow{}, h.workflowInformer)
	h.pods = h.kube.InformerFor(&corev1.Pod{}, h.podInformer)
	for _, setup := range []struct {
		informer  cache.SharedIndexInformer
		kind      resourceKind
		transform cache.TransformFunc
		name      func(any) string
	}{
		{h.workflows, workflowKind, transformWorkflow, h.workflowName},
		{h.pods, podKind, transformPod, h.podWorkflowName},
	} {
		if err := setup.informer.SetTransform(setup.transform); err != nil {
			return nil, fmt.Errorf("argowatch: set transform: %w", err)
		}
		if err := setup.informer.SetWatchErrorHandlerWithContext(h.watchErrorHandler(setup.kind)); err != nil {
			return nil, fmt.Errorf("argowatch: set watch error handler: %w", err)
		}
		if _, err := setup.informer.AddEventHandler(h.handlers(setup.name)); err != nil {
			return nil, fmt.Errorf("argowatch: add event handler: %w", err)
		}
	}
	return h, nil
}

// Run starts both factories and blocks until ctx is canceled. Call it once.
func (h *Hub) Run(ctx context.Context) {
	h.runOnce.Do(func() {
		h.argo.Start(ctx.Done())
		h.kube.Start(ctx.Done())
		ticker := time.NewTicker(resyncPeriod)
		defer ticker.Stop()
		running := true
		for running {
			select {
			case <-ctx.Done():
				running = false
			case <-ticker.C:
				h.logger.InfoContext(ctx, "argowatch: cached objects", "workflows", len(h.workflows.GetIndexer().ListKeys()), "pods", len(h.pods.GetIndexer().ListKeys()))
			}
		}
		h.healthMu.Lock()
		h.healthSinceLocked()
		h.stopped = h.now()
		h.healthSinceLocked()
		h.healthMu.Unlock()
		h.kube.Shutdown()
	})
}

// Synced reports whether both initial informer lists have completed. It stays
// true through later watch outages, separating startup from lost observation.
func (h *Hub) Synced() bool { return h.workflows.HasSynced() && h.pods.HasSynced() }

// WaitForSync waits for both informer caches, bounded only by ctx.
func (h *Hub) WaitForSync(ctx context.Context) error {
	if !cache.WaitForCacheSync(ctx.Done(), h.workflows.HasSynced, h.pods.HasSynced) {
		return fmt.Errorf("argowatch: cache sync: %w", ctx.Err())
	}
	return nil
}

// Workflow returns a shared, read-only Workflow from the cache.
func (h *Hub) Workflow(name string) (*wfv1.Workflow, bool) {
	obj, exists, err := h.workflows.GetIndexer().GetByKey(h.namespace + "/" + name)
	if err != nil || !exists || h.workflowName(obj) == "" {
		return nil, false
	}
	return obj.(*wfv1.Workflow), true
}

// Pod returns a shared, read-only step Pod from the cache.
func (h *Hub) Pod(name string) (*corev1.Pod, bool) {
	obj, exists, err := h.pods.GetIndexer().GetByKey(h.namespace + "/" + name)
	if err != nil || !exists || h.podWorkflowName(obj) == "" {
		return nil, false
	}
	return obj.(*corev1.Pod), true
}

// PodsOf returns shared, read-only step Pods indexed by Argo Workflow name.
func (h *Hub) PodsOf(workflowName string) []*corev1.Pod {
	objects, err := h.pods.GetIndexer().ByIndex(byWorkflow, workflowName)
	if err != nil {
		return nil
	}
	pods := make([]*corev1.Pod, 0, len(objects))
	for _, obj := range objects {
		if h.podWorkflowName(obj) == workflowName && workflowName != "" {
			pods = append(pods, obj.(*corev1.Pod))
		}
	}
	return pods
}

// Pods returns all shared, read-only step Pods for namespace-wide observation.
// Consumers apply their own existing selectors; this never performs API I/O.
func (h *Hub) Pods() []*corev1.Pod {
	objects := h.pods.GetIndexer().List()
	pods := make([]*corev1.Pod, 0, len(objects))
	for _, obj := range objects {
		if h.podWorkflowName(obj) != "" {
			pods = append(pods, obj.(*corev1.Pod))
		}
	}
	return pods
}

// Subscribe registers a one-slot wake channel. Cancel removes it without closing
// it and is safe to call repeatedly or concurrently with event delivery.
func (h *Hub) Subscribe(workflowName string) (wake <-chan struct{}, cancel func()) {
	ch := make(chan struct{}, 1)
	h.subMu.Lock()
	if h.subs[workflowName] == nil {
		h.subs[workflowName] = make(map[chan struct{}]struct{})
	}
	h.subs[workflowName][ch] = struct{}{}
	h.subMu.Unlock()
	return ch, func() {
		h.subMu.Lock()
		defer h.subMu.Unlock()
		delete(h.subs[workflowName], ch)
		if len(h.subs[workflowName]) == 0 {
			delete(h.subs, workflowName)
		}
	}
}

func (h *Hub) notify(name string) {
	if name == "" {
		return
	}
	h.subMu.Lock()
	defer h.subMu.Unlock()
	for ch := range h.subs[name] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (h *Hub) handlers(name func(any) string) cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) { h.notify(name(obj)) },
		UpdateFunc: func(old, current any) {
			oldName, newName := name(old), name(current)
			h.notify(oldName)
			if newName != oldName {
				h.notify(newName)
			}
		},
		DeleteFunc: func(obj any) { h.notify(name(unwrap(obj))) },
	}
}

func unwrap(obj any) any {
	switch tombstone := obj.(type) {
	case cache.DeletedFinalStateUnknown:
		return tombstone.Obj
	case *cache.DeletedFinalStateUnknown:
		return tombstone.Obj
	default:
		return obj
	}
}

func (h *Hub) workflowName(obj any) string {
	workflow, ok := obj.(*wfv1.Workflow)
	if !ok || workflow.Namespace != h.namespace || workflow.Labels["oberth.ci/engine"] != "argo" || workflow.Annotations["oberth.ci/run-id"] == "" {
		return ""
	}
	return workflow.Name
}

func (h *Hub) podWorkflowName(obj any) string {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod.Namespace != h.namespace {
		return ""
	}
	if _, ok := pod.Labels["oberth.ci/trigger"]; !ok {
		return ""
	}
	name := pod.Labels[workflowLabel]
	owner := metav1.GetControllerOf(pod)
	if name == "" || owner == nil || owner.Name != name || owner.UID == "" || owner.Kind != "Workflow" || owner.APIVersion != "argoproj.io/v1alpha1" {
		return ""
	}
	return name
}

// WorkflowsForRun returns cached Workflows carrying the durable run label.
func (h *Hub) WorkflowsForRun(runID string) []*wfv1.Workflow {
	objects, err := h.workflows.GetIndexer().ByIndex("byRun", runID)
	if err != nil {
		return nil
	}
	workflows := make([]*wfv1.Workflow, 0, len(objects))
	for _, obj := range objects {
		if wf, ok := obj.(*wfv1.Workflow); ok && wf.Namespace == h.namespace && h.workflowName(wf) != "" {
			workflows = append(workflows, wf)
		}
	}
	return workflows
}
