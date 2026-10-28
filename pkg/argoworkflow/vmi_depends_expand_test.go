package argoworkflow

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/workflow/common"
)

// Keep the upstream import test-only. The adapter also catches any future
// upstream change that starts requiring ancestry or timestamps for expansion.
type vmiArgoParityContext map[string]*wfv1.DAGTask

func (d vmiArgoParityContext) GetTask(_ context.Context, name string) *wfv1.DAGTask { return d[name] }
func (vmiArgoParityContext) GetTaskDependencies(context.Context, string) []string {
	panic("unexpected ancestry lookup")
}
func (vmiArgoParityContext) GetTaskFinishedAtTime(context.Context, string) time.Time {
	panic("unexpected timestamp lookup")
}

func TestVMIArgoDependencyExpansionParity(t *testing.T) {
	modes := []*wfv1.ContinueOn{nil, {}, {Error: true}, {Failed: true}, {Error: true, Failed: true}}
	for i, a := range modes {
		for j, b := range modes {
			t.Run(fmt.Sprintf("continue-a-%d-b-%d", i, j), func(t *testing.T) {
				tasks := vmiArgoParityContext{
					"a": {Name: "a", ContinueOn: a}, "b": {Name: "b", ContinueOn: b},
					"hyphen-name": {Name: "hyphen-name"}, "1-step": {Name: "1-step"},
					"true": {Name: "true"}, "succeeded": {Name: "succeeded"},
				}
				cases := []wfv1.DAGTask{
					{Name: "root"},
					{Name: "dependent", Dependencies: []string{"a"}},
					{Name: "dependent", Dependencies: []string{"a", "b"}},
					{Name: "dependent", Dependencies: []string{"b", "a", "a"}},
					{Name: "dependent", Depends: "a", Dependencies: []string{"missing-ignored"}},
				}
				for _, logic := range []string{
					"a", "a && b", "b || (a && b)", "a.Succeeded", "a.Succeeded && b",
					"(a.Succeeded || a.Failed || a.Errored) && b", "a.Skipped || a.Omitted || a.Daemoned",
					"a.AnySucceeded || a.AllFailed || a.Succeeded", "hyphen-name && 1-step.Succeeded",
					"true", "a.", "a.succeeded", "a.NotAStatus", " 'a' ", " a \t&&\n b ",
				} {
					cases = append(cases, wfv1.DAGTask{Name: "dependent", Depends: logic})
				}
				for _, task := range cases {
					before, err := json.Marshal([]any{task, tasks})
					if err != nil {
						t.Fatal(err)
					}
					wantRefs, want := common.GetTaskDependencies(t.Context(), &task, tasks)
					refs, got, ok := expandVMIArgoDependencies(&task, tasks)
					wantSet := map[string]bool{}
					for name := range wantRefs {
						wantSet[name] = true
					}
					if !ok || got != want || !reflect.DeepEqual(refs, wantSet) {
						t.Fatalf("expansion diverged for %+v: refs=%v logic=%q ok=%v; Argo refs=%v logic=%q", task, refs, got, ok, wantSet, want)
					}
					after, err := json.Marshal([]any{task, tasks})
					if err != nil || string(after) != string(before) {
						t.Fatal("expansion changed authored task/when/dependency bytes", err)
					}
				}
			})
		}
	}
}

func TestVMIArgoDependencyExpansionRefusesNilReferences(t *testing.T) {
	for _, task := range []*wfv1.DAGTask{nil, {Dependencies: []string{"absent"}}, {Depends: "absent"}, {Depends: "absent.Succeeded"}} {
		for _, tasks := range []map[string]*wfv1.DAGTask{nil, {"absent": nil}} {
			if _, _, ok := expandVMIArgoDependencies(task, tasks); ok {
				t.Fatalf("nil dependency accepted: %+v", task)
			}
		}
	}
	if _, ok := expandVMIArgoDependency("absent", nil); ok {
		t.Fatal("nil leaf accepted")
	}
}
