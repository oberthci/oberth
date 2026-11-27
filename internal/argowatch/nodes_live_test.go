package argowatch_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	argofake "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned/fake"
	"github.com/oberthci/oberth/internal/argowatch"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func noWatchList(action clienttesting.Action) (bool, watch.Interface, error) {
	options := action.(interface{ GetListOptions() meta.ListOptions }).GetListOptions()
	if options.SendInitialEvents != nil && *options.SendInitialEvents {
		return true, nil, apierrors.NewBadRequest("streaming list unsupported")
	}
	return false, nil, nil
}

func TestRealNodeInformerOnlyEnqueuesLinkedRuns(t *testing.T) {
	for _, total := range []int{30, 100} {
		t.Run(fmt.Sprint(total), func(t *testing.T) { synctest.Test(t, func(t *testing.T) { checkLiveNodeInformer(t, total) }) })
	}
}
func liveNodeInformer(t *testing.T, total int) (*argowatch.Hub, *kubefake.Clientset) {
	t.Helper()
	objects := []runtime.Object{&core.Node{ObjectMeta: meta.ObjectMeta{Name: "assigned", UID: "node", ResourceVersion: "1"}}}
	for i := range total {
		node := "elsewhere"
		if i < 10 {
			node = "assigned"
		}
		objects = append(objects, &core.Pod{ObjectMeta: meta.ObjectMeta{Name: fmt.Sprint("pod-", i), Namespace: "ci", Labels: map[string]string{"workflows.argoproj.io/workflow": fmt.Sprint("run-", i)}}, Spec: core.PodSpec{NodeName: node}})
	}
	kube, argo := kubefake.NewClientset(objects...), argofake.NewSimpleClientset()
	for _, resource := range []string{"pods", "nodes"} {
		kube.PrependWatchReactor(resource, noWatchList)
	}
	argo.PrependWatchReactor("workflows", noWatchList)
	h, err := argowatch.New(argowatch.Config{Namespace: "ci", Workflows: argo.ArgoprojV1alpha1().Workflows("ci"), Pods: kube.CoreV1().Pods("ci"), Nodes: kube.CoreV1().Nodes()})
	if err != nil {
		t.Fatal(err)
	}
	return h, kube
}
func checkLiveNodeInformer(t *testing.T, total int) {
	t.Helper()
	h, kube := liveNodeInformer(t, total)
	counts := map[string]int{}
	var countsMu sync.Mutex
	h.OnChange(func(name string) { countsMu.Lock(); counts[name]++; countsMu.Unlock() })
	ctx, cancel := context.WithCancel(t.Context())
	done, ready := make(chan struct{}), make(chan struct{})
	go func() { defer close(done); _ = h.Run(ctx, func() { close(ready) }) }()
	<-ready
	synctest.Wait()
	clear(counts)
	node, err := kube.CoreV1().Nodes().Get(ctx, "assigned", meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	node.ResourceVersion = "2"
	node.Status.Conditions = []core.NodeCondition{{Type: core.NodeMemoryPressure, Status: core.ConditionTrue}}
	if _, err = kube.CoreV1().Nodes().UpdateStatus(ctx, node, meta.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	synctest.Wait()
	if len(counts) != 10 {
		t.Fatal("Node update reached unrelated native observers", counts)
	}
	clear(counts)
	node.ResourceVersion = "3"
	node.Status.Conditions[0].LastHeartbeatTime = meta.NewTime(time.Unix(1800000000, 0))
	if _, err = kube.CoreV1().Nodes().UpdateStatus(ctx, node, meta.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	synctest.Wait()
	if len(counts) != 0 {
		t.Fatal("heartbeat enqueued native observers", counts)
	}
	cancel()
	<-done
}
