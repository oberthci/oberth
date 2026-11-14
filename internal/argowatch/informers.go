package argowatch

import (
	"context"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	versioned "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

func selectWorkflows(options *metav1.ListOptions) { options.LabelSelector = workflowSelector }
func selectPods(options *metav1.ListOptions)      { options.LabelSelector = podSelector }

func (h *Hub) workflowInformer(client versioned.Interface, resync time.Duration) cache.SharedIndexInformer {
	workflows := client.ArgoprojV1alpha1().Workflows(h.namespace)
	lw := h.listWatch(workflowKind, selectWorkflows,
		func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
			return workflows.List(ctx, options)
		}, workflows.Watch)
	return cache.NewSharedIndexInformer(cache.ToListWatcherWithWatchListSemantics(lw, client),
		&wfv1.Workflow{}, resync, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
			"byRun": func(obj any) ([]string, error) {
				if wf, ok := obj.(*wfv1.Workflow); ok && wf.Labels["oberth.ci/run"] != "" {
					return []string{wf.Labels["oberth.ci/run"]}, nil
				}
				return nil, nil
			},
		})
}

func (h *Hub) podInformer(client kubernetes.Interface, resync time.Duration) cache.SharedIndexInformer {
	pods := client.CoreV1().Pods(h.namespace)
	lw := h.listWatch(podKind, selectPods,
		func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
			return pods.List(ctx, options)
		}, pods.Watch)
	return cache.NewSharedIndexInformer(cache.ToListWatcherWithWatchListSemantics(lw, client),
		&corev1.Pod{}, resync, cache.Indexers{
			cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
			byWorkflow: func(obj any) ([]string, error) {
				if name := h.podWorkflowName(obj); name != "" {
					return []string{name}, nil
				}
				return nil, nil
			},
		})
}

// Only observe attempts. The reflector owns retries, resourceVersions, watch
// bookmarks, streaming-list fallback and relists; none are interpreted here.
func (h *Hub) listWatch(kind resourceKind, selectOptions func(*metav1.ListOptions), list cache.ListWithContextFunc, watchFn cache.WatchFuncWithContext) *cache.ListWatch {
	return &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
			selectOptions(&options)
			obj, err := list(ctx, options)
			h.recordAttempt(kind, err)
			return obj, err
		},
		WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
			selectOptions(&options)
			stream, err := watchFn(ctx, options)
			h.recordAttempt(kind, err)
			return stream, err
		},
	}
}

func transformWorkflow(obj any) (any, error) {
	if workflow, ok := obj.(*wfv1.Workflow); ok {
		workflow.ManagedFields = nil
		workflow.Status.StoredTemplates = nil
		workflow.Status.StoredWorkflowSpec = nil
	}
	return obj, nil
}

func transformPod(obj any) (any, error) {
	if pod, ok := obj.(*corev1.Pod); ok {
		pod.ManagedFields = nil
	}
	return obj, nil
}
