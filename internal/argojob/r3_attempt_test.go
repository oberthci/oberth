package argojob

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"testing"
)

func TestAttemptMergeRetainsRunningAndAbsentPodName(t *testing.T) {
	t.Parallel()
	old := stepmodel.RunStep{Attempts: []stepmodel.StepAttempt{{NodeID: "n", PodName: "saved-pod", Status: stepmodel.StepRunning}}}
	current := stepmodel.RunStep{Attempts: []stepmodel.StepAttempt{{NodeID: "n", Status: stepmodel.StepQueued}}}
	got := mergeAttempts(current, old, nil)
	if len(got) != 1 || got[0].Status != stepmodel.StepRunning || got[0].PodName != "saved-pod" {
		t.Fatal(got)
	}
}
func TestRawAttemptOrdinalsOverrideDisplayPositions(t *testing.T) {
	t.Parallel()
	out := Observation{}
	in := projectionInput{w: &wf.Workflow{}, out: &out, paths: nativePaths{"old": nativePath{order: []int{0}}, "new": nativePath{order: []int{7}}}, previousNodes: []stateport.Node{{NodeID: "old", Attempt: 7}}}
	row := projected{step: stepmodel.RunStep{Path: "build"}}
	projectAttempt(&row, in, wf.NodeStatus{ID: "old"})
	projectAttempt(&row, in, wf.NodeStatus{ID: "new"})
	if out.Nodes[0].Attempt != 7 || out.Nodes[1].Attempt != 8 {
		t.Fatal(out.Nodes)
	}
}
func TestLogSelectorMergeDeduplicatesPhysicalSelection(t *testing.T) {
	t.Parallel()
	selector := stepmodel.LogSelector{NodeID: "node", PodName: "pod", Container: "main"}
	got := mergeLogs([]stepmodel.LogSelector{selector, selector}, []stepmodel.LogSelector{selector})
	if len(got) != 1 || got[0] != selector {
		t.Fatal(got)
	}
}
func TestUntrackedCaptureOffersAreIgnored(t *testing.T) {
	t.Parallel()
	e := New(Config{})
	defer e.queue.ShutDown()
	tr := &tracking{binding: nativebinding.RunBinding{Workflow: nativebinding.Object{Name: "run"}}, captures: map[string]bool{}}
	e.offerCapture(tr, captureJob{entries: []runlog.ContainerLog{{Step: "step", PodUID: "pod", Container: "main"}}})
	if len(tr.pending) != 0 {
		t.Fatal("untracked capture accepted", tr.pending)
	}
	e.tracked["run"] = &tracking{}
	e.offerCapture(tr, captureJob{entries: []runlog.ContainerLog{{Step: "step", PodUID: "pod", Container: "main"}}})
	if len(tr.pending) != 0 {
		t.Fatal("replaced tracking accepted an old capture", tr.pending)
	}
}
