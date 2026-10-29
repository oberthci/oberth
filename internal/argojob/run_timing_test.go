package argojob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/testtimeout"
)

func TestTimingPaginatedLargeDAG(t *testing.T) {
	wf, node, pod, _ := diagnosticFixture()
	wf.Status.Nodes = wfv1.Nodes{}
	pods := []corev1.Pod{}
	for i := 0; i < 65; i++ {
		n := *node.DeepCopy()
		n.ID = fmt.Sprintf("workflow-test-%d", 1000+i)
		n.Name = fmt.Sprintf("workflow-test.task%d", i)
		n.DisplayName = fmt.Sprintf("task%d", i)
		wf.Status.Nodes[n.ID] = n
		p := pod.DeepCopy()
		p.Name = PodName(wf, n)
		p.UID = types.UID(fmt.Sprintf("22222222-2222-4222-8222-%012d", i))
		p.Annotations["workflows.argoproj.io/node-id"] = n.ID
		p.Annotations["workflows.argoproj.io/node-name"] = n.Name
		pods = append(pods, *p)
	}
	kube := fake.NewClientset()
	calls := 0
	kube.PrependReactor("list", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		opts := action.(ktesting.ListActionImpl).GetListOptions()
		if opts.Limit != 128 || opts.LabelSelector != "workflows.argoproj.io/workflow="+wf.Name {
			t.Fatalf("unbounded scope: %+v", opts)
		}
		start := calls * 33
		calls++
		if calls > 2 || (calls == 1 && opts.Continue != "") || (calls == 2 && opts.Continue != "next") {
			t.Fatalf("wrong page: %+v", opts)
		}
		end := min(start+33, len(pods))
		cursor := ""
		if end < len(pods) {
			cursor = "next"
		}
		// A slow API costs one round trip per page, independent of leaf count.
		time.Sleep(20 * time.Millisecond)
		return true, &corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "7", Continue: cursor}, Items: pods[start:end]}, nil
	})
	c := &Controller{kube: kube}
	completion := completionFor(wf)
	start := time.Now()
	c.collectStepDiagnostics(context.Background(), wf, &completion)
	if calls != 2 || len(completion.Steps) != 65 || time.Since(start) > testtimeout.Start(time.Second) {
		t.Fatalf("collection calls=%d steps=%d time=%s", calls, len(completion.Steps), time.Since(start))
	}
	for _, step := range completion.Steps {
		if !step.Termination.Available || step.Timing == nil || !step.Timing.Available || step.Timing.Validate() != nil {
			t.Fatalf("lost timing: %+v", step)
		}
	}
}

func TestTimingIncompleteSnapshotNeverProjectsPartialEvidence(t *testing.T) {
	for _, mode := range []string{"api-error", "cursor-cycle", "resource-version", "duplicate", "page-bound"} {
		t.Run(mode, func(t *testing.T) {
			wf, _, pod, _ := diagnosticFixture()
			kube := fake.NewClientset()
			calls := 0
			kube.PrependReactor("list", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
				calls++
				list := &corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "7", Continue: fmt.Sprint(calls)}}
				if calls == 1 {
					list.Items = []corev1.Pod{*pod}
					return true, list, nil
				}
				switch mode {
				case "api-error":
					return true, nil, errors.New("synthetic private API failure")
				case "cursor-cycle":
					list.Continue = "1"
				case "resource-version":
					list.ResourceVersion = "8"
				case "duplicate":
					list.Items = []corev1.Pod{*pod}
					list.Continue = ""
				}
				return true, list, nil
			})
			c := &Controller{kube: kube}
			completion := completionFor(wf)
			c.collectStepDiagnostics(context.Background(), wf, &completion)
			if calls > 16 {
				t.Fatal("page budget exceeded")
			}
			for _, s := range completion.Steps {
				if s.Termination.Available || s.Termination.Unavailable != "snapshot-incomplete" || s.Timing.Available || s.Timing.Unavailable != "snapshot-incomplete" {
					t.Fatalf("partial snapshot accepted: %+v", s)
				}
			}
		})
	}
}

func TestTimingRejectsCrossAttemptPodBindings(t *testing.T) {
	for _, mode := range []string{"owner", "node-id", "node-name", "pod-name", "namespace", "workflow-uid"} {
		t.Run(mode, func(t *testing.T) {
			wf, node, pod, at := diagnosticFixture()
			switch mode {
			case "owner":
				pod.OwnerReferences[0].UID = "33333333-3333-4333-8333-333333333333"
			case "node-id":
				pod.Annotations["workflows.argoproj.io/node-id"] = "other"
			case "node-name":
				pod.Annotations["workflows.argoproj.io/node-name"] = "other"
			case "pod-name":
				pod.Name = "other"
			case "namespace":
				pod.Namespace = "other"
			case "workflow-uid":
				wf.UID = "33333333-3333-4333-8333-333333333333"
			}
			step := StepResults(wf)[0]
			d := projectStepDiagnostic(wf, node, pod, at)
			v := projectStepTiming(wf, node, pod, step, d, at)
			if v.Available || v.Main != nil || v.PodCreatedAt != nil || len(v.Init) != 0 || v.Unavailable != "binding-mismatch" || v.Validate() != nil {
				t.Fatalf("accepted %s: %+v", mode, v)
			}
		})
	}
}

func TestTimingInitAndLogicalReadinessAreExplicit(t *testing.T) {
	wf, node, pod, at := diagnosticFixture()
	boundary := wfv1.NodeStatus{ID: "root", TemplateName: "dag", StartedAt: metav1.NewTime(at.Add(-2 * time.Minute))}
	node.BoundaryID = boundary.ID
	node.DisplayName = "work"
	wf.Status.Nodes[boundary.ID] = boundary
	wf.Status.Nodes[node.ID] = node
	wf.Spec.Entrypoint = "dag"
	wf.Spec.Templates = []wfv1.Template{{Name: "dag", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "work", Template: "leaf"}}}}, {Name: "leaf", Container: &corev1.Container{Name: "main", Command: []string{"secret-canary"}}}}
	pod.CreationTimestamp = metav1.NewTime(at.Add(-90 * time.Second))
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(at.Add(-80 * time.Second))}}
	cid := "containerd://" + strings.Repeat("d", 64)
	pod.Spec.InitContainers = []corev1.Container{{Name: "executor-token"}}
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "executor-token", ContainerID: cid, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ContainerID: cid, StartedAt: metav1.NewTime(at.Add(-79 * time.Second)), FinishedAt: metav1.NewTime(at.Add(-74 * time.Second))}}}}
	v := projectStepTiming(wf, node, pod, StepResults(wf)[0], projectStepDiagnostic(wf, node, pod, at), at)
	if !v.Available || v.DependencyReadyAt == nil || v.ScheduledAt == nil || len(v.Init) != 1 || v.Init[0].FinishedAt.Sub(v.Init[0].StartedAt) != 5*time.Second || v.ScriptTiming != "mixed" || v.ImagePullTiming != "not-observed" {
		t.Fatalf("timing %+v", v)
	}
	graph, err := timingGraph(wf)
	if err != nil || len(graph) != 1 || graph[0].ID != "work" {
		t.Fatalf("graph=%+v err=%v", graph, err)
	}
	raw, _ := json.Marshal(graph)
	if strings.Contains(string(raw), "secret-canary") {
		t.Fatal("script leaked")
	}
	wf.Spec.Templates[0].DAG.Tasks[0].Depends = "secret-canary.Succeeded"
	if dependencyReady(wf, node) != nil {
		t.Fatal("expression inferred")
	}
	graph, err = timingGraph(wf)
	raw, _ = json.Marshal(graph)
	if err != nil || graph[0].Readiness != "expression-unavailable" || strings.Contains(string(raw), "secret-canary") {
		t.Fatal("raw expression retained")
	}
}

var _ TimingRecorder = (*timingRecorderStub)(nil)

type timingRecorderStub struct {
	writeErr  error
	workflows []model.WorkflowTiming
	steps     []model.StepTiming
}

func (r *timingRecorderStub) RecordRunTimingMilestone(context.Context, string, string) error {
	return nil
}
func (r *timingRecorderStub) RecordWorkflowTiming(_ context.Context, v model.WorkflowTiming) error {
	r.workflows = append(r.workflows, v)
	return nil
}
func (r *timingRecorderStub) RecordStepTimings(ctx context.Context, v []model.StepTiming) error {
	r.writeErr = ctx.Err()
	r.steps = append(r.steps, v...)
	return nil
}

func TestTimingGraphBoundsKeepWorkflowBinding(t *testing.T) {
	for _, mode := range []string{"tasks", "bytes"} {
		t.Run(mode, func(t *testing.T) {
			wf, _, _, at := diagnosticFixture()
			wf.CreationTimestamp = metav1.NewTime(at)
			wf.Annotations[identityAnnotation] = strings.Repeat("c", 64)
			wf.Spec.Entrypoint = "dag"
			dag := &wfv1.DAGTemplate{}
			count := model.MaxTimingTasks + 1
			if mode == "bytes" {
				count = 200
			}
			for i := 0; i < count; i++ {
				task := wfv1.DAGTask{Name: fmt.Sprintf("task-%040d", i), Template: "leaf"}
				if mode == "bytes" {
					for j := 0; j < i; j++ {
						task.Dependencies = append(task.Dependencies, fmt.Sprintf("task-%040d", j))
					}
				}
				dag.Tasks = append(dag.Tasks, task)
			}
			wf.Spec.Templates = []wfv1.Template{{Name: "dag", DAG: dag}, {Name: "leaf", Container: &corev1.Container{Name: "main"}}}
			recorder := &timingRecorderStub{}
			controller := &Controller{timings: recorder}
			controller.retainWorkflowTiming(context.Background(), wf)
			if len(recorder.workflows) != 1 {
				t.Fatal("binding discarded with graph")
			}
			retained := recorder.workflows[0]
			if retained.Validate() != nil || retained.UID != string(wf.UID) || retained.GraphUnavailable == "" || len(retained.Tasks) != 0 {
				t.Fatalf("graph bounds lost valid binding: %+v", retained)
			}
		})
	}
}
