package argoworkflow

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
)

func TestPreparationAndInspectionKnowTheSameStampedParameters(t *testing.T) {
	w := sourceWorkflow()
	r := requestFor(t, w)
	r.Trigger, r.ScheduleEntry = "schedule", "nightly"
	p := Policy{SecretstoreAddress: "https://store.invalid"}
	prepared, err := prepareDocumentation(t, r, p, identityHooks())
	if err != nil {
		t.Fatal(err)
	}
	for _, parameter := range prepared.View().Workflow.Spec.Arguments.Parameters {
		w.Spec.Templates[0].Container.Args = append(w.Spec.Templates[0].Container.Args, "{{workflow.parameters."+parameter.Name+"}}")
	}
	r.Document, _ = json.Marshal(w)
	r.Mode, r.IdentityRequest.Mode, r.RunID = admissioncontract.ModeInspect, admissioncontract.ModeInspect, ""
	inspected, err := Prepare(t.Context(), r, Policy{}, admissioncontract.PrepareHooks{OfflineValidation: true})
	if err != nil {
		t.Fatal("preparation stamped a parameter inspection does not know", err)
	}
	for _, parameter := range inspected.View().Workflow.Spec.Arguments.Parameters {
		if parameter.Value == nil || parameter.Value.String() == "" {
			t.Fatal("empty placeholder", parameter)
		}
	}
}

func TestRemovedParametersHaveIndividualRemediesAlongsideOtherFaults(t *testing.T) {
	w := simpleWorkflow()
	w.Spec.PodSpecPatch = "{}"
	w.Spec.Templates[0].Container.Args = nil
	names := []string{"oberth-sha", "oberth-ref", "oberth-repo", "oberth-release-tag", "oberth-release-sha"}
	for _, name := range names {
		w.Spec.Templates[0].Container.Args = append(w.Spec.Templates[0].Container.Args, "{{workflow.parameters."+name+"}}")
	}
	r := requestFor(t, w)
	var first []admissioncontract.Finding
	for _, mode := range []admissioncontract.Mode{admissioncontract.ModeSubmit, admissioncontract.ModeInspect} {
		r.Mode, r.IdentityRequest.Mode = mode, mode
		_, err := Prepare(t.Context(), r, Policy{}, identityHooks())
		got := admissionFindings(t, err)
		if len(got) != 6 {
			t.Fatal("missing or duplicate findings", got)
		}
		for i, name := range names {
			if got[i+1].Code != name || !strings.Contains(got[i+1].Reason, name) || got[i+1].Remedy != messages()[name].remedy {
				t.Fatal(got)
			}
		}
		if first == nil {
			first = got
		} else if !reflect.DeepEqual(first, got) {
			t.Fatal("push and inspect disagree", first, got)
		}
	}
}

func TestNeedsSurviveAlongsideNativeAndPolicyRefusals(t *testing.T) {
	w, fixture := tokenWorkflow()
	w.Spec.PodSpecPatch = "{}"
	w.Spec.Templates = append(w.Spec.Templates, wf.Template{Name: "bad", Container: w.Spec.Templates[0].Container.DeepCopy(), Suspend: &wf.SuspendTemplate{}})
	r := requestFor(t, w)
	r.Mode, r.IdentityRequest.Mode = admissioncontract.ModeInspect, admissioncontract.ModeInspect
	_, err := Prepare(t.Context(), r, fixture.Policy, admissioncontract.PrepareHooks{})
	var refused *AdmissionError
	if !errors.As(err, &refused) || len(refused.Findings) < 2 || len(refused.Needs) == 0 {
		t.Fatal("requirements or independent refusals lost", err)
	}
	if refused.Needs[0].Command == "" {
		t.Fatal("grant lost its command", refused.Needs)
	}
}

func TestMalformedRequirementsRemainRefusalsOffline(t *testing.T) {
	for _, change := range []func(*wf.Workflow){
		func(w *wf.Workflow) { w.Annotations = map[string]string{SecretPathsAnnotation: "../secret"} },
		func(w *wf.Workflow) {
			w.Spec.Templates[0].Container.Env = nil
			w.Spec.Templates[0].Container.Image = ""
		},
	} {
		w := simpleWorkflow()
		change(w)
		r := requestFor(t, w)
		r.Mode, r.IdentityRequest.Mode = admissioncontract.ModeInspect, admissioncontract.ModeInspect
		if _, err := Prepare(t.Context(), r, Policy{}, admissioncontract.PrepareHooks{}); err == nil {
			t.Fatal("authored fault reported only as a need")
		}
	}
}

func TestNativeReportsEveryMissingArgumentAndUnresolvedField(t *testing.T) {
	w := simpleWorkflow()
	w.Spec.Arguments.Parameters = []wf.Parameter{{Name: "first"}, {Name: "second"}}
	w.Spec.Templates[0].Container.Args = []string{"{{workflow.parameters.third}}", "{{workflow.parameters.fourth}}"}
	_, err := Prepare(t.Context(), requestFor(t, w), Policy{}, identityHooks())
	findings := admissionFindings(t, err)
	if len(findings) != 4 {
		t.Fatal("independent fields hidden", err)
	}
}

func TestNativeDoesNotReportAResolvedReferenceInAnotherTemplate(t *testing.T) {
	w := simpleWorkflow()
	w.Spec.Templates[0].Container.Args = []string{"{{inputs.parameters.value}}"}
	good := *w.Spec.Templates[0].DeepCopy()
	good.Name = "good"
	good.Inputs.Parameters = []wf.Parameter{{Name: "value"}}
	w.Spec.Templates = append(w.Spec.Templates, good)
	_, err := Prepare(t.Context(), requestFor(t, w), Policy{}, identityHooks())
	if findings := admissionFindings(t, err); len(findings) != 1 || strings.Contains(findings[0].Path, "good") {
		t.Fatal("valid reference was reported", err)
	}
}

func TestMissingLiveTemplateDoesNotHideIndependentNativeFault(t *testing.T) {
	w := sharedConsumer("shared", "work")
	bad := simpleWorkflow().Spec.Templates[0]
	bad.Name = "bad"
	bad.Container.Args = []string{"{{workflow.parameters.missing}}"}
	w.Spec.Templates = append(w.Spec.Templates, bad)
	r := requestFor(t, w)
	r.Mode, r.IdentityRequest.Mode = admissioncontract.ModeInspect, admissioncontract.ModeInspect
	_, err := Prepare(t.Context(), r, Policy{}, admissioncontract.PrepareHooks{})
	var refused *AdmissionError
	if !errors.As(err, &refused) || len(refused.Findings) != 1 || len(refused.Needs) == 0 || !strings.Contains(refused.Findings[0].Path, "bad") {
		t.Fatal("live reference hid an independent fault", err)
	}
}

func TestNativeDiagnosticsDoNotMutateInputAndCollectAllHooks(t *testing.T) {
	w := simpleWorkflow()
	w.Spec.Arguments.Parameters = []wf.Parameter{{Name: "first"}, {Name: "second"}}
	w.Spec.Hooks = wf.LifecycleHooks{"running": {Template: "main"}, "failed": {Template: "main"}}
	before, _ := json.Marshal(w)
	for range 20 {
		err := ValidateNativeSchema(t.Context(), w, admissioncontract.PrepareRequest{SelectedFile: ".oberth/build.yaml"})
		if findings := admissionFindings(t, err); len(findings) != 4 {
			t.Fatal("native errors hidden", err)
		}
		after, _ := json.Marshal(w)
		if string(before) != string(after) {
			t.Fatal("native diagnostics repaired their input")
		}
	}
}
