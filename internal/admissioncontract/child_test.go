package admissioncontract

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"reflect"
	"testing"
)

func TestScheduledChildPreservesFrozenPlanAndAuthority(t *testing.T) {
	v := preparedFixture()
	v.RunID = ""
	v.Trigger = "schedule"
	v.ScheduleEntry = "nightly"
	v.SourceBindings = nil
	v.Identity.Namespace = "ci"
	v = mustSeal(t, v).View()
	child := v.Workflow.DeepCopy()
	child.Name = "child-123"
	child.Namespace = "ci"
	child.Status.Phase = wf.WorkflowFailed
	p, err := v.ScheduledChild(child)
	if err != nil {
		t.Fatal(err)
	}
	got := p.View()
	if got.RunID != child.Name || got.Digest == v.Digest || got.SpecDigest != v.SpecDigest || got.Workflow.Status.Phase != "" || !reflect.DeepEqual(got.Plan, v.Plan) || !reflect.DeepEqual(got.Grants, v.Grants) || !reflect.DeepEqual(got.Templates, v.Templates) {
		t.Fatal("child changed frozen admission", got)
	}
	got.Plan.Entries[0].Name = "changed"
	if p.View().Plan.Entries[0].Name == "changed" {
		t.Fatal("child admission aliases view")
	}
}

func TestScheduledChildRefusesUnboundOrChangedAdmission(t *testing.T) {
	for name, change := range map[string]func(*PreparedView, **wf.Workflow){
		"nil_child":   func(_ *PreparedView, w **wf.Workflow) { *w = nil },
		"name":        func(_ *PreparedView, w **wf.Workflow) { (*w).Name = "" },
		"run":         func(v *PreparedView, _ **wf.Workflow) { v.RunID = "other" },
		"trigger":     func(v *PreparedView, _ **wf.Workflow) { v.Trigger = "branch" },
		"entry":       func(v *PreparedView, _ **wf.Workflow) { v.ScheduleEntry = "" },
		"source":      func(v *PreparedView, _ **wf.Workflow) { v.SourceBindings = []sourcecontract.Binding{{}} },
		"digest":      func(v *PreparedView, _ **wf.Workflow) { v.Digest = "forged" },
		"plan":        func(v *PreparedView, _ **wf.Workflow) { v.Plan.Exact = !v.Plan.Exact },
		"spec_digest": func(v *PreparedView, _ **wf.Workflow) { v.SpecDigest = "forged" },
		"spec":        func(_ *PreparedView, w **wf.Workflow) { (*w).Spec.Entrypoint = "other" },
		"namespace":   func(_ *PreparedView, w **wf.Workflow) { (*w).Namespace = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			v := preparedFixture()
			v.RunID = ""
			v.Trigger = "schedule"
			v.ScheduleEntry = "nightly"
			v.SourceBindings = nil
			v.Identity.Namespace = "ci"
			v = mustSeal(t, v).View()
			child := v.Workflow.DeepCopy()
			child.Name = "child-123"
			child.Namespace = "ci"
			change(&v, &child)
			if _, err := v.ScheduledChild(child); err == nil {
				t.Fatal("changed admission accepted")
			}
		})
	}
}

func TestScheduledChildAllowsShutdownWithoutChangingAdmittedSpec(t *testing.T) {
	for _, admitted := range []wf.ShutdownStrategy{"", wf.ShutdownStrategyStop} {
		for _, observed := range []wf.ShutdownStrategy{"", wf.ShutdownStrategyStop, wf.ShutdownStrategyTerminate} {
			t.Run(string(admitted)+"/"+string(observed), func(t *testing.T) {
				v := preparedFixture()
				v.Trigger, v.ScheduleEntry = "schedule", "nightly"
				v.RunID, v.SourceBindings = "", nil
				v.Identity.Namespace = "ci"
				v.Workflow.Spec.Shutdown = admitted
				v = mustSeal(t, v).View()
				child := v.Workflow.DeepCopy()
				child.Name, child.Namespace = "child-123", "ci"
				child.Spec.Shutdown = observed
				before := child.DeepCopy()
				sealed, err := v.ScheduledChild(child)
				if err != nil {
					t.Fatal("native shutdown prevented child binding", err)
				}
				got := sealed.View()
				if got.SpecDigest != v.SpecDigest || !reflect.DeepEqual(got.Workflow.Spec, v.Workflow.Spec) {
					t.Fatal("native shutdown changed the sealed spec")
				}
				if !reflect.DeepEqual(child, before) {
					t.Fatal("child binding rewrote observed shutdown")
				}
			})
		}
	}
}
