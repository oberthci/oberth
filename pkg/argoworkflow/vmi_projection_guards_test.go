package argoworkflow

import (
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func TestVMICompileMissingDependenciesNeverPanics(t *testing.T) {
	for _, tc := range []struct {
		name string
		dag  *wfv1.DAGTemplate
	}{
		{name: "nil-dag"},
		{name: "missing-legacy", dag: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "dependent", Dependencies: []string{"missing"}}}}},
		{name: "missing-bare", dag: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "dependent", Depends: "missing"}}}},
		{name: "missing-status", dag: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "dependent", Depends: "missing.Succeeded"}}}},
		{name: "missing-second-legacy", dag: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "known"}, {Name: "dependent", Dependencies: []string{"known", "missing"}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("compile must refuse missing input without panic: %v", r)
				}
			}()
			if compileVMIArgoDependencies(tc.dag) {
				t.Fatal("missing dependency accepted")
			}
		})
	}
	if !compileVMIArgoDependencies(&wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "dependent", Dependencies: []string{"later"}}, {Name: "later"}}}) {
		t.Fatal("valid forward reference refused")
	}
}

func TestVMIWhitespaceOnlyWhenMatchesArgoError(t *testing.T) {
	for _, guard := range []string{" ", "\t", "\n\r", " \t\r\n ", "\u00a0", "\u2003"} {
		t.Run(guard, func(t *testing.T) {
			if _, err := reviewArgoWhen(t, guard, nil); err == nil {
				t.Fatal("pinned Argo unexpectedly accepted whitespace-only when")
			}
			condition := parseVMICondition(guard, RunTriggerBranch, func(string) (vmiScalar, bool) { return vmiScalar{}, false }, 0)
			if !condition.unsafe || condition.truth != vmiUnknown {
				t.Fatalf("whitespace must be unsupported, not an empty guard: %+v", condition)
			}
			wf, binding, policy := reviewWhenWorkflow(t, "bare", "delete", guard)
			if _, supported := runVMIPeak(wf, binding.Trigger); supported {
				t.Fatal("invalid guard earned lifetime credit")
			}
			checkVMISoundnessDemand(t, wf, binding, policy, 2)
		})
	}
	for _, guard := range []string{"", " true ", "\ttrue\n"} {
		condition := parseVMICondition(guard, RunTriggerBranch, func(string) (vmiScalar, bool) { return vmiScalar{}, false }, 0)
		if condition.unsafe || condition.truth != vmiTrue {
			t.Fatalf("empty/valid padded control refused: %q %+v", guard, condition)
		}
	}
}
