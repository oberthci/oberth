package app_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

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
	w := workflow()
	steps := wfv1.ParallelSteps{}
	for i := range 40 {
		name := fmt.Sprintf("pin%d", i)
		inner := workflow()
		addSource(inner, pin)
		template := inner.Spec.Templates[0]
		template.Name = name
		w.Spec.Templates = append(w.Spec.Templates, template)
		steps.Steps = append(steps.Steps, wfv1.WorkflowStep{Name: name, Template: name})
	}
	w.Spec.Templates[0] = wfv1.Template{Name: "build", Steps: []wfv1.ParallelSteps{steps}}
	f.setDocument(w)
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
