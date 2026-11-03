package argoworkflow

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/argoproj/argo-workflows/v4/util/expr/argoexpr"
)

type reviewTaskResults struct {
	Succeeded, Failed, Errored, Skipped, Omitted, Daemoned, AnySucceeded, AllFailed bool
}

func TestReviewExprKeywordTaskNames(t *testing.T) {
	for _, name := range []string{"in", "not", "and", "or", "matches", "contains", "startswith", "nil", "true", "false", "let", "if", "else", "len", "all", "any", "none", "one", "filter", "map", "count", "abs", "int", "string", "keys", "now", "env", "get", "max", "type", "find", "sum", "date", "trim", "first", "last", "join", "split", "upper", "lower", "repeat", "sort", "float", "duration", "toJSON", "fromJSON", "has", "ceil", "floor", "round", "median", "mean", "min", "uniq", "concat", "flatten", "reduce", "groupby", "sortby", "timezone", "bitand", "bitor", "toPairs", "fromPairs", "base64", "hasprefix", "hassuffix", "indexof", "lastindexof", "toBase64", "fromBase64", "x", "in-step", "step-in", "1-step", "step-1"} {
		evalName := strings.ReplaceAll(name, "-", "_")
		env := map[string]reviewTaskResults{evalName: {Succeeded: false, Failed: true}}
		got, err := argoexpr.EvalBool(evalName+".Succeeded", env)
		if err != nil {
			t.Logf("%-10s ERROR: %v", name, err)
		} else {
			t.Logf("%-10s -> %v", name, got)
		}
		wf, _, _ := vmiSoundnessWorkflow(t)
		wf.Spec.Templates = append(wf.Spec.Templates, wfv1.Template{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
			{Name: "create-first", Template: "create-a"},
			{Name: name, Template: "delete-a", Depends: "create-first.Succeeded"},
			{Name: "after-delete", Template: "neutral", Depends: name + ".Succeeded"},
			{Name: "create-next", Template: "create-b", Depends: "after-delete.Succeeded || after-delete.Failed || after-delete.Errored"},
		}}})
		peak, supported := runVMIPeak(wf, RunTriggerBranch)
		if err != nil || !vmiDNSLabelPattern.MatchString(name) {
			if supported {
				t.Errorf("Argo-invalid result identifier %q earned a lifetime proof: %+v", name, peak)
			}
		} else if !supported || peak.VMs != 1 {
			t.Errorf("valid (including builtin-shadowing) result identifier %q lost its proof: %+v %v", name, peak, supported)
		}
	}
}

// Argo v4.0.8 evaluateDependsLogic: the dependent of "in" errors regardless of
// the delete's phase; the 3-state successor of x then evaluates true.
func TestReviewKeywordDependsArgoSemantics(t *testing.T) {
	_, err := argoexpr.EvalBool("in.Succeeded", map[string]reviewTaskResults{"in": {Failed: true}})
	t.Logf("x depends in.Succeeded (delete FAILED): err=%v  => x marked Errored", err)
	if err == nil {
		t.Fatal("Argo unexpectedly accepted the keyword-named dependency")
	}
	ok, err := argoexpr.EvalBool("x.Succeeded || x.Failed || x.Errored", map[string]reviewTaskResults{"x": {Errored: true}})
	t.Logf("b depends x.Succeeded || x.Failed || x.Errored with x Errored: %v err=%v => create-b runs while create-a is live", ok, err)
	if err != nil || !ok {
		t.Fatalf("terminal-state successor did not run: %v %v", ok, err)
	}
}
