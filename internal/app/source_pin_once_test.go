package app_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func pinnedWorkflow(pins ...sourcecontract.Descriptor) *wfv1.Workflow {
	w := workflow()
	steps := wfv1.ParallelSteps{}
	paths := map[sourcecontract.Descriptor]string{}
	for i, pin := range pins {
		name := fmt.Sprintf("pin%d", i)
		inner := workflow()
		addSource(inner, pin)
		if paths[pin] == "" {
			paths[pin] = fmt.Sprintf("/work/sources/lib%d", len(paths))
		}
		inner.Spec.Templates[0].Inputs.Artifacts[0].Path = paths[pin]
		template := inner.Spec.Templates[0]
		template.Name = name
		w.Spec.Templates = append(w.Spec.Templates, template)
		steps.Steps = append(steps.Steps, wfv1.WorkflowStep{Name: name, Template: name})
	}
	w.Spec.Templates[0] = wfv1.Template{Name: "build", Steps: []wfv1.ParallelSteps{steps}}
	return w
}

// A pipeline declares a pin once per template that uses it; the own
// pipelines declare the same producer commit dozens of times. One admission
// decides each (repository, commit) once and reuses the decision for every
// declaration, so its cost is one head read per producer commit, not one per
// declaration.
func TestOneAdmissionDecidesEachPinnedCommitOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.d.SourcePolicy = []byte(`{"origin/team/widget":["origin/team/source"]}`)
	pin := sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Pinned, Repository: "origin/team/source", Commit: strings.Repeat("e", 40)}
	pins := make([]sourcecontract.Descriptor, 40)
	for i := range pins {
		pins[i] = pin
	}
	f.setDocument(pinnedWorkflow(pins...))
	var reads atomic.Int32
	f.source.Reachable = func(context.Context, sourcecontract.Revision) (int64, bool, error) {
		reads.Add(1)
		return 2, true, nil
	}
	p, err := f.new(t).Admit(context.Background(), request(), f.source)
	if err != nil {
		t.Fatal("the admission was refused", err)
	}
	if got := len(p.View().SourceBindings); got < 40 {
		t.Fatal("not every declaration was bound", got)
	}
	if got := reads.Load(); got != 1 {
		t.Fatal("the pinned commit was decided once per declaration", got)
	}
}

func TestPinMemoIsPerAdmissionAndPerCommit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.d.SourcePolicy = []byte(`{"origin/team/widget":["origin/team/source"]}`)
	first := sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Pinned, Repository: "origin/team/source", Commit: strings.Repeat("e", 40)}
	second := first
	second.Commit = strings.Repeat("f", 40)
	f.setDocument(pinnedWorkflow(first, second, first, second))
	var mu sync.Mutex
	reads := map[sourcecontract.Revision]int{}
	reachable := true
	f.source.Reachable = func(_ context.Context, at sourcecontract.Revision) (int64, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		reads[at]++
		return 2, reachable || at.Commit == first.Commit, nil
	}
	s := f.new(t)
	if _, err := s.Admit(t.Context(), request(), f.source); err != nil {
		t.Fatal(err)
	}
	for _, pin := range []sourcecontract.Descriptor{first, second} {
		if n := reads[sourcecontract.Revision{Repository: pin.Repository, Commit: pin.Commit}]; n != 1 {
			t.Fatal("first admission did not decide each commit once", reads)
		}
	}
	reachable = false
	prepared, err := s.Admit(t.Context(), request(), f.source)
	expectRefusal(t, err, "reachable")
	if prepared != nil {
		t.Fatal("unreachable commit inherited another commit's approval", err)
	}
	for _, pin := range []sourcecontract.Descriptor{first, second} {
		if n := reads[sourcecontract.Revision{Repository: pin.Repository, Commit: pin.Commit}]; n != 2 {
			t.Fatal("second admission reused a decision or repeated a declaration", reads)
		}
	}
}
