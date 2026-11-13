package app

import (
	"context"
	"testing"
	"time"

	argofake "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/oberthci/oberth/internal/argowatch"
)

func TestArgoJobsWatchSyncIsShutdownBoundedAndDelegatesLocalLifecycle(t *testing.T) {
	kube := kubefake.NewClientset()
	argo := argofake.NewSimpleClientset()
	argo.PrependWatchReactor("workflows", func(action clienttesting.Action) (bool, watch.Interface, error) {
		options := action.(interface{ GetListOptions() metav1.ListOptions }).GetListOptions()
		if options.SendInitialEvents != nil && *options.SendInitialEvents {
			return true, nil, apierrors.NewBadRequest("streaming lists unsupported")
		}
		return false, nil, nil
	})
	hub, err := argowatch.New(kube, argo, "pipeline", nil)
	if err != nil {
		t.Fatal(err)
	}
	local := &ArgoJobs{watchHub: hub}
	jobs := &ArgoJobs{routes: &executionTargets{local: local}}
	// Before the informer starts, a caller waits until its own shutdown.
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := jobs.WaitForWatchSync(canceled); err == nil {
		t.Fatal("unsynced cache accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); jobs.RunWatch(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	wait, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	if err := jobs.WaitForWatchSync(wait); err != nil || !hub.Healthy() {
		t.Fatal("local cache lifecycle not delegated", err)
	}
}
