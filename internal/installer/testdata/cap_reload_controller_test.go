package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/argoproj/argo-workflows/v4/config"
	"github.com/argoproj/argo-workflows/v4/util/logging"
	wfsync "github.com/argoproj/argo-workflows/v4/workflow/sync"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

type capObservedThrottler struct {
	wfsync.Throttler
	applied chan int
}

func (c *capObservedThrottler) UpdateParallelism(value int) {
	c.Throttler.UpdateParallelism(value)
	c.applied <- value
}

func TestOberthCapConfigReload(t *testing.T) {
	raw, err := os.ReadFile(os.Getenv("OBERTH_CAP_CONFIGS"))
	if err != nil {
		t.Fatal(err)
	}
	var configs [2]corev1.ConfigMap
	if err = json.Unmarshal(raw, &configs); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	ctx = logging.TestContext(ctx)
	configs[0].ResourceVersion, configs[1].ResourceVersion = "1", "2"
	client := fake.NewClientset(&configs[0])
	allowSelfSubjectAccessReviews(client)
	watching := capConfigWatch(client)
	throttler := &capObservedThrottler{Throttler: wfsync.NewMultiThrottler(0, 0, func(wfsync.Key) {}), applied: make(chan int, 2)}
	c := &WorkflowController{kubeclientset: client, configController: config.NewController(configs[0].Namespace, configs[0].Name, client), throttler: throttler}
	informer, synced := c.newControllerConfigMapInformer(ctx)
	if informer == nil {
		t.Fatal("controller cannot watch its configuration")
	}
	done := make(chan struct{})
	go func() { defer close(done); informer.Run(ctx.Done()) }()
	t.Cleanup(func() { cancel(); <-done })
	if !cache.WaitForCacheSync(ctx.Done(), synced) {
		t.Fatal("configuration informer did not synchronize")
	}
	capApplied(t, ctx, throttler.applied, 6)
	for i := range 8 {
		throttler.Add(fmt.Sprintf("jobs/run-%d", i), 0, time.Date(2026, 11, 16, 12, i, 0, 0, time.UTC))
	}
	capAdmitted(t, throttler, 6)
	select {
	case <-watching:
	case <-ctx.Done():
		t.Fatal("configuration watch did not start")
	}
	if _, err = client.CoreV1().ConfigMaps(configs[1].Namespace).Update(ctx, &configs[1], metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	capApplied(t, ctx, throttler.applied, 8)
	capAdmitted(t, throttler, 8)
	t.Log("one controller instance: cap 6 to 8, admitted runs 6 to 8")
}

func capConfigWatch(client *fake.Clientset) <-chan struct{} {
	watching := make(chan struct{})
	var once sync.Once
	client.PrependWatchReactor("configmaps", func(action ktesting.Action) (bool, watch.Interface, error) {
		stream, err := client.Tracker().Watch(action.GetResource(), action.GetNamespace())
		if err != nil {
			return false, nil, err
		}
		once.Do(func() { close(watching) })
		return true, stream, nil
	})
	return watching
}

func capApplied(t *testing.T, ctx context.Context, values <-chan int, want int) {
	t.Helper()
	select {
	case got := <-values:
		if got != want {
			t.Fatal("controller loaded the wrong run cap", got, want)
		}
	case <-ctx.Done():
		t.Fatal("controller did not reload the run cap")
	}
}

func capAdmitted(t *testing.T, throttler wfsync.Throttler, want int) {
	t.Helper()
	admitted := 0
	for i := range 8 {
		if throttler.Admit(fmt.Sprintf("jobs/run-%d", i)) {
			admitted++
		}
	}
	if admitted != want {
		t.Fatal("controller did not apply parallelism to its native throttle", admitted, want)
	}
}
