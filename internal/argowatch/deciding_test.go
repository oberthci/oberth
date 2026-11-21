package argowatch_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/oberthci/oberth/internal/argowatch"
)

type watchCalls struct {
	stream  *watch.RaceFreeFakeWatcher
	options chan meta.ListOptions
}

func (c watchCalls) Watch(_ context.Context, options meta.ListOptions) (watch.Interface, error) {
	if options.SendInitialEvents != nil && *options.SendInitialEvents {
		return nil, apierrors.NewBadRequest("streaming list unsupported")
	}
	c.options <- options
	if c.stream.IsStopped() {
		return watch.NewRaceFreeFake(), nil
	}
	return c.stream, nil
}

type workflowLists struct {
	watchCalls
	list func(context.Context, meta.ListOptions) (*wf.WorkflowList, error)
}

func (c workflowLists) List(ctx context.Context, options meta.ListOptions) (*wf.WorkflowList, error) {
	return c.list(ctx, options)
}

type podLists struct {
	watchCalls
	items []core.Pod
}

func (c podLists) List(context.Context, meta.ListOptions) (*core.PodList, error) {
	return &core.PodList{ListMeta: meta.ListMeta{ResourceVersion: "1"}, Items: c.items}, nil
}

func newWatchCalls() watchCalls {
	return watchCalls{stream: watch.NewRaceFreeFake(), options: make(chan meta.ListOptions, 8)}
}
func startWatchHub(t *testing.T, config argowatch.Config) (*argowatch.Hub, context.Context, <-chan string) {
	t.Helper()
	h, err := argowatch.New(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	ready, done := make(chan struct{}), make(chan error, 1)
	changes := make(chan string, 32)
	h.OnChange(func(name string) { changes <- name })
	go func() { done <- h.Run(ctx, func() { close(ready) }) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("informer did not synchronize", ctx.Err())
	}
	return h, ctx, changes
}
func watchOption(t *testing.T, ctx context.Context, options <-chan meta.ListOptions) meta.ListOptions {
	t.Helper()
	select {
	case got := <-options:
		return got
	case <-ctx.Done():
		t.Fatal("watch did not start", ctx.Err())
	}
	return meta.ListOptions{}
}
func changedName(t *testing.T, ctx context.Context, changes <-chan string) string {
	t.Helper()
	select {
	case name := <-changes:
		return name
	case <-ctx.Done():
		t.Fatal("workflow notification missing", ctx.Err())
	}
	return ""
}

func TestWatchSelectorsAndPodReassignment(t *testing.T) {
	workflows := workflowLists{watchCalls: newWatchCalls(), list: func(context.Context, meta.ListOptions) (*wf.WorkflowList, error) {
		return &wf.WorkflowList{ListMeta: meta.ListMeta{ResourceVersion: "1"}}, nil
	}}
	pod := core.Pod{ObjectMeta: meta.ObjectMeta{Name: "physical", Namespace: "ci", ResourceVersion: "1", Labels: map[string]string{"workflows.argoproj.io/workflow": "before"}}}
	pods := podLists{watchCalls: newWatchCalls(), items: []core.Pod{pod}}
	h, ctx, changes := startWatchHub(t, argowatch.Config{Namespace: "ci", Workflows: workflows, Pods: pods})
	if got := watchOption(t, ctx, workflows.options).LabelSelector; got != "oberth.ci/engine=argo" {
		t.Fatal("workflow watch lost selector", got)
	}
	if got := watchOption(t, ctx, pods.options).LabelSelector; got != "workflows.argoproj.io/workflow" {
		t.Fatal("pod watch lost selector", got)
	}
	if got := changedName(t, ctx, changes); got != "before" {
		t.Fatal("initial pod notification", got)
	}
	next := pod.DeepCopy()
	next.ResourceVersion = "2"
	next.Labels["workflows.argoproj.io/workflow"] = "after"
	pods.stream.Modify(next)
	for _, want := range []string{"before", "after"} {
		if got := changedName(t, ctx, changes); got != want {
			t.Fatal("reassignment lost affected workflow", got, want)
		}
	}
	if len(h.Pods("before")) != 0 || len(h.Pods("after")) != 1 {
		t.Fatal("reassignment did not move indexed pod")
	}
}

func TestWorkflowWatchFailureAndRelistDeletion(t *testing.T) {
	relisting, resume := make(chan struct{}), make(chan struct{})
	var lists atomic.Int32
	workflows := workflowLists{watchCalls: newWatchCalls()}
	workflows.list = func(ctx context.Context, _ meta.ListOptions) (*wf.WorkflowList, error) {
		if lists.Add(1) == 1 {
			return &wf.WorkflowList{ListMeta: meta.ListMeta{ResourceVersion: "1"}, Items: []wf.Workflow{{ObjectMeta: meta.ObjectMeta{Name: "gone", Namespace: "ci", ResourceVersion: "1", Labels: map[string]string{"oberth.ci/engine": "argo"}}}}}, nil
		}
		if lists.Load() == 2 {
			close(relisting)
		}
		select {
		case <-resume:
			return &wf.WorkflowList{ListMeta: meta.ListMeta{ResourceVersion: "2"}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	pods := podLists{watchCalls: newWatchCalls()}
	h, ctx, changes := startWatchHub(t, argowatch.Config{Namespace: "ci", Workflows: workflows, Pods: pods})
	_ = watchOption(t, ctx, workflows.options)
	_ = watchOption(t, ctx, pods.options)
	if got := changedName(t, ctx, changes); got != "gone" || !h.Healthy() {
		t.Fatal("initial watch not healthy", got)
	}
	workflows.stream.Error(&meta.Status{Status: meta.StatusFailure, Reason: meta.StatusReasonExpired, Code: 410})
	select {
	case <-relisting:
	case <-ctx.Done():
		t.Fatal("expired workflow watch did not relist")
	}
	if h.Healthy() {
		t.Error("workflow watch outage ignored while pod watch stayed healthy")
	}
	close(resume)
	if got := changedName(t, ctx, changes); got != "gone" {
		t.Fatal("relist deletion lost workflow", got)
	}
	if _, ok := h.Workflow("gone"); ok {
		t.Fatal("relist retained missing workflow")
	}
}
