package argowatch_test

import (
	"context"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	argofake "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned/fake"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/oberthci/oberth/internal/argowatch"
)

func TestInformerSelectorsCopiesEventsAndCancellation(t *testing.T) {
	w := &wf.Workflow{ObjectMeta: meta.ObjectMeta{Name: "run", Namespace: "ci", ResourceVersion: "1", Labels: map[string]string{"oberth.ci/engine": "argo"}, ManagedFields: []meta.ManagedFieldsEntry{{Manager: "native"}}}}
	foreign := w.DeepCopy()
	foreign.Name = "foreign"
	foreign.Labels = nil
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "pod", Namespace: "ci", Labels: map[string]string{"workflows.argoproj.io/workflow": "run"}}}
	argo, kube := argofake.NewSimpleClientset(w, foreign), kubefake.NewClientset(pod)
	argo.PrependWatchReactor("workflows", func(a clienttesting.Action) (bool, watch.Interface, error) {
		options := a.(interface{ GetListOptions() meta.ListOptions }).GetListOptions()
		if options.SendInitialEvents != nil && *options.SendInitialEvents {
			return true, nil, apierrors.NewBadRequest("streaming list unsupported")
		}
		return false, nil, nil
	})
	kube.PrependWatchReactor("pods", func(a clienttesting.Action) (bool, watch.Interface, error) {
		options := a.(interface{ GetListOptions() meta.ListOptions }).GetListOptions()
		if options.SendInitialEvents != nil && *options.SendInitialEvents {
			return true, nil, apierrors.NewBadRequest("streaming list unsupported")
		}
		return false, nil, nil
	})
	h, err := argowatch.New(argowatch.Config{Namespace: "ci", Workflows: argo.ArgoprojV1alpha1().Workflows("ci"), Pods: kube.CoreV1().Pods("ci")})
	if err != nil {
		t.Fatal(err)
	}
	changes := make(chan string, 32)
	h.OnChange(func(name string) { changes <- name })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ready, done := make(chan struct{}), make(chan error, 1)
	go func() { done <- h.Run(ctx, func() { close(ready) }) }()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatalf("informer not ready: workflow=%v pods=%d argo=%v kube=%v", func() bool { _, ok := h.Workflow("run"); return ok }(), len(h.Pods("run")), argo.Actions(), kube.Actions())
	}
	assertCache(t, h)
	var got *wf.Workflow
	w.ResourceVersion = "2"
	w.Status.Phase = wf.WorkflowRunning
	if _, err = argo.ArgoprojV1alpha1().Workflows("ci").Update(ctx, w, meta.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	observed := false
	for !observed {
		select {
		case <-changes:
			got, _ = h.Workflow("run")
			observed = got.ResourceVersion == "2"
		case <-ctx.Done():
			t.Fatal("change not delivered")
		}
	}
	cancel()
	<-done
}

func assertCache(t *testing.T, h *argowatch.Hub) {
	t.Helper()
	if !h.Healthy() {
		t.Fatal("synced watches unhealthy")
	}
	got, ok := h.Workflow("run")
	if !ok || len(got.ManagedFields) != 0 {
		t.Fatal("transform failed")
	}
	got.Labels["changed"] = "yes"
	other, _ := h.Workflow("run")
	if other.Labels["changed"] != "" {
		t.Fatal("mutable cache escaped")
	}
	if _, ok = h.Workflow("foreign"); ok {
		t.Fatal("selector admitted foreign Workflow")
	}
	pods := h.Pods("run")
	if len(pods) != 1 {
		t.Fatal(pods)
	}
	pods[0].Name = "changed"
	if h.Pods("run")[0].Name != "pod" {
		t.Fatal("mutable Pod escaped")
	}
}

func TestPodAtNamesTheNewestUnfinishedPod(t *testing.T) {
	pod := func(uid string, age int64, phase core.PodPhase, ips ...string) *core.Pod {
		p := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: uid, Namespace: "ci", UID: types.UID(uid), CreationTimestamp: meta.Unix(1_800_000_000-age, 0), Labels: map[string]string{"workflows.argoproj.io/workflow": "run"}}, Status: core.PodStatus{Phase: phase}}
		for _, ip := range ips {
			p.Status.PodIPs = append(p.Status.PodIPs, core.PodIP{IP: ip})
		}
		if len(ips) > 0 {
			p.Status.PodIP = ips[0]
		}
		return p
	}
	kube := kubefake.NewClientset(pod("flooder", 60, core.PodFailed, "10.0.0.5"), pod("older", 90, core.PodRunning, "10.0.0.5"), pod("successor", 1, core.PodPending, "10.0.0.5"), pod("dual", 5, core.PodRunning, "10.0.0.6", "fd00::6"), pod("done", 5, core.PodSucceeded, "10.0.0.7"))
	kube.PrependWatchReactor("pods", func(a clienttesting.Action) (bool, watch.Interface, error) {
		options := a.(interface{ GetListOptions() meta.ListOptions }).GetListOptions()
		if options.SendInitialEvents != nil && *options.SendInitialEvents {
			return true, nil, apierrors.NewBadRequest("streaming list unsupported")
		}
		return false, nil, nil
	})
	argo := argofake.NewSimpleClientset()
	argo.PrependWatchReactor("workflows", func(a clienttesting.Action) (bool, watch.Interface, error) {
		options := a.(interface{ GetListOptions() meta.ListOptions }).GetListOptions()
		if options.SendInitialEvents != nil && *options.SendInitialEvents {
			return true, nil, apierrors.NewBadRequest("streaming list unsupported")
		}
		return false, nil, nil
	})
	h, err := argowatch.New(argowatch.Config{Namespace: "ci", Workflows: argo.ArgoprojV1alpha1().Workflows("ci"), Pods: kube.CoreV1().Pods("ci")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ready, done := make(chan struct{}), make(chan error, 1)
	go func() { done <- h.Run(ctx, func() { close(ready) }) }()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("informer not ready")
	}
	for ip, want := range map[string]string{"10.0.0.5": "successor", "10.0.0.6": "dual", "fd00::6": "dual", "10.0.0.7": "", "10.0.0.8": ""} {
		if got := h.PodAt(ip); got != want {
			t.Error("address resolved to the wrong Pod", ip, got, want)
		}
	}
	cancel()
	<-done
}
