package argowatch

import (
	"context"
	"sync"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
)

// Workflows and Pods are the list/watch subset of namespace-scoped native clients.
type Workflows interface {
	List(context.Context, meta.ListOptions) (*wf.WorkflowList, error)
	Watch(context.Context, meta.ListOptions) (watch.Interface, error)
}
type Pods interface {
	List(context.Context, meta.ListOptions) (*core.PodList, error)
	Watch(context.Context, meta.ListOptions) (watch.Interface, error)
}
type Config struct {
	Workflows Workflows
	Pods      Pods
	Namespace string
}
type Hub struct {
	workflows, pods cache.SharedIndexInformer
	mu              sync.RWMutex
	changed         func(string)
	healthy         [2]bool
	namespace       string
}

func New(c Config) (*Hub, error) {
	h := &Hub{namespace: c.Namespace}
	var err error
	h.workflows, err = h.informer(informerInput{index: 0, client: c.Workflows, selector: "oberth.ci/engine=argo", object: &wf.Workflow{}, list: func(ctx context.Context, o meta.ListOptions) (runtime.Object, error) { return c.Workflows.List(ctx, o) }, watch: c.Workflows.Watch})
	if err != nil {
		return nil, err
	}
	h.pods, err = h.informer(informerInput{index: 1, client: c.Pods, selector: "workflows.argoproj.io/workflow", object: &core.Pod{}, list: func(ctx context.Context, o meta.ListOptions) (runtime.Object, error) { return c.Pods.List(ctx, o) }, watch: c.Pods.Watch})
	if err != nil {
		return nil, err
	}
	return h, nil
}

type informerInput struct {
	client   any
	index    int
	selector string
	object   runtime.Object
	list     func(context.Context, meta.ListOptions) (runtime.Object, error)
	watch    func(context.Context, meta.ListOptions) (watch.Interface, error)
}

func (h *Hub) informer(in informerInput) (cache.SharedIndexInformer, error) {
	lw := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, o meta.ListOptions) (runtime.Object, error) {
			o.LabelSelector = in.selector
			result, err := in.list(ctx, o)
			h.health(in.index, err == nil)
			return result, err
		},
		WatchFuncWithContext: func(ctx context.Context, o meta.ListOptions) (watch.Interface, error) {
			o.LabelSelector = in.selector
			result, err := in.watch(ctx, o)
			h.health(in.index, err == nil)
			return result, err
		},
	}
	informer := cache.NewSharedIndexInformer(cache.ToListWatcherWithWatchListSemantics(lw, in.client), in.object, resyncPeriod, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc, "workflow": workflowIndex, "ip": podIPIndex})
	if err := informer.SetTransform(func(obj any) (any, error) {
		if m, ok := obj.(meta.Object); ok {
			m.SetManagedFields(nil)
		}
		return obj, nil
	}); err != nil {
		return nil, err
	}
	if err := informer.SetWatchErrorHandlerWithContext(func(_ context.Context, _ *cache.Reflector, _ error) { h.health(in.index, false) }); err != nil {
		return nil, err
	}
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: h.change, UpdateFunc: func(a, b any) { h.change(a); h.change(b) }, DeleteFunc: h.change})
	return informer, err
}
func (h *Hub) health(index int, healthy bool) { h.mu.Lock(); h.healthy[index] = healthy; h.mu.Unlock() }
func (h *Hub) Healthy() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.workflows.HasSynced() && h.pods.HasSynced() && h.healthy[0] && h.healthy[1]
}
func (h *Hub) OnChange(fn func(string)) { h.mu.Lock(); h.changed = fn; h.mu.Unlock() }
func (h *Hub) change(obj any) {
	if dead, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = dead.Obj
	}
	name := ""
	switch o := obj.(type) {
	case *wf.Workflow:
		name = o.Name
	case *core.Pod:
		name = o.Labels["workflows.argoproj.io/workflow"]
	}
	h.mu.RLock()
	fn := h.changed
	h.mu.RUnlock()
	if fn != nil && name != "" {
		fn(name)
	}
}

// Run waits for both initial lists before signaling readiness, then joins both informers.
func (h *Hub) Run(ctx context.Context, ready func()) error {
	var wg sync.WaitGroup
	for _, i := range []cache.SharedIndexInformer{h.workflows, h.pods} {
		wg.Go(func() { i.RunWithContext(ctx) })
	}
	defer wg.Wait()
	if !cache.WaitForCacheSync(ctx.Done(), h.workflows.HasSynced, h.pods.HasSynced) {
		return ctx.Err()
	}
	ready()
	<-ctx.Done()
	return ctx.Err()
}
func (h *Hub) Workflow(name string) (*wf.Workflow, bool) {
	obj, ok, err := h.workflows.GetIndexer().GetByKey(h.namespace + "/" + name)
	if err != nil || !ok {
		return nil, false
	}
	return obj.(*wf.Workflow).DeepCopy(), true
}
func (h *Hub) Pods(workflow string) []*core.Pod {
	objects, err := h.pods.GetIndexer().ByIndex("workflow", workflow)
	if err != nil {
		return nil
	}
	out := make([]*core.Pod, 0, len(objects))
	for _, obj := range objects {
		out = append(out, obj.(*core.Pod).DeepCopy())
	}
	return out
}

// PodAt names the UID of the newest unfinished Pod holding ip, or "" when the
// watch holds none; a finished Pod's recycled address names its successor.
func (h *Hub) PodAt(ip string) string {
	objects, err := h.pods.GetIndexer().ByIndex("ip", ip)
	if err != nil {
		return ""
	}
	var newest *core.Pod
	for _, obj := range objects {
		p := obj.(*core.Pod)
		if p.Status.Phase == core.PodSucceeded || p.Status.Phase == core.PodFailed {
			continue
		}
		if newest == nil || newest.CreationTimestamp.Before(&p.CreationTimestamp) || newest.CreationTimestamp.Equal(&p.CreationTimestamp) && p.UID > newest.UID {
			newest = p
		}
	}
	if newest == nil {
		return ""
	}
	return string(newest.UID)
}

const resyncPeriod = 10 * time.Minute

func workflowIndex(obj any) ([]string, error) {
	if p, ok := obj.(*core.Pod); ok {
		return []string{p.Labels["workflows.argoproj.io/workflow"]}, nil
	}
	return nil, nil
}

func podIPIndex(obj any) ([]string, error) {
	p, ok := obj.(*core.Pod)
	if !ok {
		return nil, nil
	}
	ips := make([]string, 0, len(p.Status.PodIPs)+1)
	if p.Status.PodIP != "" {
		ips = append(ips, p.Status.PodIP)
	}
	for _, ip := range p.Status.PodIPs {
		if ip.IP != "" && ip.IP != p.Status.PodIP {
			ips = append(ips, ip.IP)
		}
	}
	return ips, nil
}
