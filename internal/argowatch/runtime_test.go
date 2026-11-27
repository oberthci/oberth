package argowatch

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"
	"testing"
	"time"
)

// Synced informers expose independent health bits after both initial lists.
type syncedInformer struct{ cache.SharedIndexInformer }

func (syncedInformer) HasSynced() bool { return true }
func TestResyncHealthAndPodNotification(t *testing.T) {
	if resyncPeriod != 10*time.Minute {
		t.Fatal(resyncPeriod)
	}
	h := &Hub{workflows: syncedInformer{}, pods: syncedInformer{}, healthy: [3]bool{true, true}}
	if !h.Healthy() {
		t.Fatal("both healthy")
	}
	h.health(1, false)
	if h.Healthy() {
		t.Fatal("pod outage ignored")
	}
	names := []string{}
	h.OnChange(func(name string) { names = append(names, name) })
	h.change(&core.Pod{ObjectMeta: meta.ObjectMeta{Name: "physical", Labels: map[string]string{"workflows.argoproj.io/workflow": "logical"}}})
	if len(names) != 1 || names[0] != "logical" {
		t.Fatal(names)
	}
}
func TestIndexedReadsSelectWorkflow(t *testing.T) {
	makeInformer := func(obj runtime.Object) cache.SharedIndexInformer {
		return cache.NewSharedIndexInformer(&cache.ListWatch{}, obj, 0, cache.Indexers{"workflow": workflowIndex})
	}
	w, p := makeInformer(&wf.Workflow{}), makeInformer(&core.Pod{})
	_ = w.GetIndexer().Add(&wf.Workflow{ObjectMeta: meta.ObjectMeta{Name: "wanted", Namespace: "ci"}})
	_ = p.GetIndexer().Add(&core.Pod{ObjectMeta: meta.ObjectMeta{Name: "a", Labels: map[string]string{"workflows.argoproj.io/workflow": "wanted"}}})
	_ = p.GetIndexer().Add(&core.Pod{ObjectMeta: meta.ObjectMeta{Name: "b", Labels: map[string]string{"workflows.argoproj.io/workflow": "other"}}})
	h := &Hub{workflows: w, pods: p, namespace: "ci"}
	if _, ok := h.Workflow("wanted"); !ok {
		t.Fatal("key lookup")
	}
	if rows := h.Pods("wanted"); len(rows) != 1 || rows[0].Name != "a" {
		t.Fatal(rows)
	}
}
