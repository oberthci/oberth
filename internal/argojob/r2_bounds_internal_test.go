package argojob

import (
	"fmt"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stateport"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"testing"
	"time"
)

func TestEvidenceRequiresTerminalAndPhysicalPodCoverage(t *testing.T) {
	t.Parallel()
	e := New(Config{Now: func() time.Time { return time.Unix(100, 0) }})
	defer e.queue.ShutDown()
	w := &wf.Workflow{ObjectMeta: meta.ObjectMeta{Name: "w", Namespace: "ci", UID: "workflow"}, Status: wf.WorkflowStatus{Nodes: wf.Nodes{"n": {ID: "n", Name: "w.main", Type: wf.NodeTypePod}}}}
	o := Observation{workflow: w, Complete: true, Nodes: []stateport.Node{{NodeID: "n"}}}
	if e.evidenceSettled(&tracking{}, Observation{}, nil) {
		t.Fatal("settled before terminal")
	}
	if e.evidenceSettled(&tracking{completed: true}, o, nil) {
		t.Fatal("settled without a Pod")
	}
	p := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "pod", Namespace: "ci", UID: "pod-uid", Labels: map[string]string{"workflows.argoproj.io/workflow": "w"}, Annotations: map[string]string{"workflows.argoproj.io/node-id": "n", "workflows.argoproj.io/node-name": "w.main"}, OwnerReferences: []meta.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: "w", UID: "workflow", Controller: new(true)}}}, Spec: core.PodSpec{Containers: []core.Container{{Name: "main"}}}}
	if e.evidenceSettled(&tracking{completed: true}, o, []*core.Pod{p}) {
		t.Fatal("settled before container evidence")
	}
	p.Status.ContainerStatuses = []core.ContainerStatus{{Name: "main", State: core.ContainerState{Terminated: &core.ContainerStateTerminated{}}}}
	if !e.evidenceSettled(&tracking{completed: true}, o, []*core.Pod{p}) {
		t.Fatal("complete evidence never settled")
	}
}
func TestCapturePendingReplacesPerStepAndUntrackDiscards(t *testing.T) {
	t.Parallel()
	e := New(Config{})
	defer e.queue.ShutDown()
	for i := range 1000 {
		name := string(rune(i + 1000))
		b := nativebinding.RunBinding{Workflow: nativebinding.Object{Name: name}}
		tr := &tracking{binding: b, captures: map[string]bool{}}
		e.tracked[name] = tr
		for _, pod := range []string{"old", "new", "new"} {
			e.offerCapture(tr, captureJob{binding: b, entries: []runlog.ContainerLog{{Step: "step", PodUID: pod, Container: "main"}}})
		}
		if len(tr.pending) != 1 || tr.pending["step"].entries[0].PodUID != "new" {
			t.Fatal("pending did not coalesce", tr.pending)
		}
		e.Untrack(name)
		if _, ok := e.nextCapture(); ok {
			t.Fatal("untracked pending survived")
		}
	}
	b := nativebinding.RunBinding{Workflow: nativebinding.Object{Name: "run"}}
	tr := &tracking{binding: b, captures: map[string]bool{}}
	e.tracked["run"] = tr
	job := captureJob{binding: b, entries: []runlog.ContainerLog{{Step: "step", PodUID: "pod", Container: "main"}}}
	e.offerCapture(tr, job)
	if _, ok := e.nextCapture(); !ok {
		t.Fatal("capture lost")
	}
	e.offerCapture(tr, job)
	if _, ok := e.nextCapture(); ok {
		t.Fatal("physical container captured twice")
	}
}
func TestProcessUntracksSettledAndLostRuns(t *testing.T) {
	t.Parallel()
	e := New(Config{Now: func() time.Time { return time.Unix(100, 0) }})
	defer e.queue.ShutDown()
	e.tracked["finished"] = &tracking{binding: nativebinding.RunBinding{Workflow: nativebinding.Object{Name: "finished"}}, lost: true, completed: true, evidenceUntil: time.Unix(160, 0)}
	e.process(t.Context(), "finished")
	if len(e.tracked) != 0 {
		t.Fatal("finished tracking retained")
	}
}

func TestPendingStepCaptureRetainsAllRetryContainers(t *testing.T) {
	t.Parallel()
	logs, err := runlog.Open(t.Context(), t.TempDir(), 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := logs.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	e := New(Config{Logs: logs})
	defer e.queue.ShutDown()
	w := &wf.Workflow{ObjectMeta: meta.ObjectMeta{Name: "w", Namespace: "ci", UID: "workflow"}, Status: wf.WorkflowStatus{Nodes: wf.Nodes{}}}
	b := nativebinding.RunBinding{RunID: "run", Workflow: nativebinding.Object{Name: w.Name}}
	tr := &tracking{binding: b, captures: map[string]bool{}}
	e.tracked[w.Name] = tr
	parent := wf.NodeStatus{ID: "retry", Name: "w.step", Type: wf.NodeTypeRetry}
	pods := []*core.Pod{}
	for i := range 3 {
		id := fmt.Sprint(i)
		name := "w.step(" + id + ")"
		parent.Children = append(parent.Children, id)
		w.Status.Nodes[id] = wf.NodeStatus{ID: id, Name: name, Type: wf.NodeTypePod}
		pods = append(pods, &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "pod-" + id, Namespace: "ci", UID: types.UID("uid-" + id), Labels: map[string]string{"workflows.argoproj.io/workflow": "w"}, Annotations: map[string]string{"workflows.argoproj.io/node-id": id, "workflows.argoproj.io/node-name": name}, OwnerReferences: []meta.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: "w", UID: "workflow", Controller: new(true)}}}, Status: core.PodStatus{ContainerStatuses: []core.ContainerStatus{{Name: "main", State: core.ContainerState{Terminated: &core.ContainerStateTerminated{}}}}}})
	}
	w.Status.Nodes[parent.ID] = parent
	e.capture(t.Context(), tr, w, pods)
	e.capture(t.Context(), tr, w, pods)
	if len(tr.pending) != 1 || len(tr.pending["step"].entries) != 3 {
		t.Fatal("retry logs discarded during coalescing", tr.pending)
	}
}
