package argoworkflow

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/runtimeconfig"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

type priorityCase struct {
	class    string
	priority int32
	want     int
}

func TestNativePrioritiesAreAdvisoryAndNeverRewriteSerialWork(t *testing.T) {
	for _, mode := range []admissioncontract.Mode{admissioncontract.ModeInspect, admissioncontract.ModeSubmit} {
		for _, tc := range []priorityCase{
			{"oberth-run-release-oberth", 30, 0}, {"oberth-run-promotion-oberth", 20, 0},
			{"oberth-run-qualification-oberth", 10, 0}, {"oberth-run-lint-oberth", 0, 0},
			{"oberth-run-release-oberth", 0, 1}, {"custom", 17, 1}, {"", 30, 1},
			{"oberth-control-oberth", 0, 0}, {"oberth-control-elsewhere", 0, 1},
			{"oberth-control-custom", 17, 1},
		} {
			t.Run(string(mode)+"/"+tc.class, func(t *testing.T) { checkNativePriority(t, mode, tc) })
		}
	}
}

func TestControlPriorityWithoutInstallationIsUnmanaged(t *testing.T) {
	g := graph{}
	g.runPriority("oberth-control-", nil, "spec.podPriorityClassName", false)
	assertPriorityFindings(t, g.findings, 1)
}

func checkNativePriority(t *testing.T, mode admissioncontract.Mode, tc priorityCase) {
	t.Helper()
	w := simpleWorkflow()
	serial := int64(1)
	w.Spec.Priority, w.Spec.PodPriorityClassName, w.Spec.Parallelism = &tc.priority, tc.class, &serial
	w.Spec.Templates[0].PriorityClassName = "oberth-run-lint-oberth"
	r := requestFor(t, w)
	r.Mode, r.IdentityRequest.Mode = mode, mode
	prepared, err := Prepare(t.Context(), r, Policy{ServerNamespace: "oberth"}, identityHooks())
	if err != nil || prepared == nil {
		t.Fatal("native priority refused admission", mode, tc, err)
	}
	view := prepared.View()
	if !reflect.DeepEqual(w.Spec.Priority, view.Workflow.Spec.Priority) || view.Workflow.Spec.PodPriorityClassName != tc.class || *view.Workflow.Spec.Parallelism != 1 || view.Workflow.Spec.Templates[0].PriorityClassName != "oberth-run-lint-oberth" {
		t.Fatal("native ordering rewrote authored priority or serialization")
	}
	assertPriorityFindings(t, view.Findings, tc.want)
}

func assertPriorityFindings(t *testing.T, findings []admissioncontract.Finding, want int) {
	t.Helper()
	if len(findings) != want {
		t.Fatal("priority advisory count", want, findings)
	}
	for _, f := range findings {
		if !f.Advisory || f.Code != "native_priority" || len(f.Reason) > 40 || len(f.Remedy) > 120 {
			t.Fatal("priority diagnostic is not bounded advisory", f)
		}
	}
}

func TestPriorityAdvisoriesUseMergedNativeDefaults(t *testing.T) {
	w := simpleWorkflow()
	r := requestFor(t, w)
	raw := json.RawMessage(`{"spec":{"priority":30,"podPriorityClassName":"oberth-run-release-oberth"}}`)
	canonical, err := sourcecontract.CanonicalJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	r.Defaults = runtimeconfig.WorkflowDefaultsSnapshot{WorkflowDefaults: raw, Digest: sourcecontract.Digest(canonical)}
	p, err := Prepare(t.Context(), r, Policy{ServerNamespace: "oberth"}, identityHooks())
	if err != nil || p == nil {
		t.Fatal(err)
	}
	if len(p.View().Findings) != 0 || *p.View().Workflow.Spec.Priority != 30 || p.View().Workflow.Spec.PodPriorityClassName != "oberth-run-release-oberth" {
		t.Fatal("native priority defaults lost or inspected before merging", p.View())
	}
}
