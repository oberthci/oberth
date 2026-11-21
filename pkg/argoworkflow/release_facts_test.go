package argoworkflow

import (
	"context"
	"encoding/json"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runtimeconfig"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func TestReleaseFactsRejectEachAuthoredArgumentAndGlobalOutput(t *testing.T) {
	for name := range (admissioncontract.ReleaseFacts{}).Parameters() {
		for _, carrier := range []string{"argument", "default", "valueFrom", "global-output", "workflow-default"} {
			t.Run(name+"/"+carrier, func(t *testing.T) {
				checkAuthoredReleaseParameter(t, name, carrier)
			})
		}
	}
}

func TestOnlyVerifiedReleaseGetsFrozenServerParameters(t *testing.T) {
	for _, release := range []bool{true, false} {
		checkVerifiedReleaseParameters(t, release)
	}
}

func checkAuthoredReleaseParameter(t *testing.T, name, carrier string) {
	t.Helper()
	w := simpleWorkflow()
	p := wf.Parameter{Name: name, Value: wf.AnyStringPtr("forged")}
	switch carrier {
	case "default":
		p.Value, p.Default = nil, wf.AnyStringPtr("forged")
	case "valueFrom":
		p.Value, p.ValueFrom = nil, &wf.ValueFrom{Expression: "'forged'"}
	case "global-output":
		p.Name, p.GlobalName = "output", name
		w.Spec.Templates[0].Outputs.Parameters = []wf.Parameter{p}
	}
	if carrier != "global-output" && carrier != "workflow-default" {
		w.Spec.Arguments.Parameters = []wf.Parameter{p}
	}
	r := requestFor(t, w)
	if carrier == "workflow-default" {
		raw, err := json.Marshal(wf.Workflow{Spec: wf.WorkflowSpec{Arguments: wf.Arguments{Parameters: []wf.Parameter{p}}}})
		if err != nil {
			t.Fatal(err)
		}
		raw, err = sourcecontract.CanonicalJSON(json.RawMessage(raw))
		if err != nil {
			t.Fatal(err)
		}
		r.Defaults = runtimeconfig.WorkflowDefaultsSnapshot{WorkflowDefaults: raw, Digest: sourcecontract.Digest(raw)}
	}
	hooks := identityHooks()
	hooks.ReleaseFacts = func(context.Context) (admissioncontract.ReleaseFacts, error) {
		t.Fatal("facts read before authored refusal")
		return admissioncontract.ReleaseFacts{}, nil
	}
	r.Trigger = model.TriggerTag
	policy := submitPolicy()
	policy.Release = true
	_, err := prepareFixture(t.Context(), r, policy, hooks)
	requireCode(t, err, "release_parameter")
}

func checkVerifiedReleaseParameters(t *testing.T, release bool) {
	t.Helper()
	r := requestFor(t, simpleWorkflow())
	r.Ref, r.Trigger, r.SelectedFile = "refs/tags/v1.2.3", model.TriggerTag, ".oberth/release.yaml"
	r.SourceReceipt.File = r.SelectedFile
	policy, hooks := submitPolicy(), identityHooks()
	policy.Release = release
	calls := 0
	facts := admissioncontract.ReleaseFacts{Version: "v1.2.3", TestedCommit: r.CommitSHA, DefaultCommit: r.CommitSHA, BuildStatus: "passed"}
	hooks.ReleaseFacts = func(context.Context) (admissioncontract.ReleaseFacts, error) { calls++; return facts, nil }
	prepared, err := prepareFixture(t.Context(), r, policy, hooks)
	if err != nil {
		t.Fatal(err)
	}
	count := checkReleaseParameterValues(t, prepared.View().Workflow, facts)
	if release && (count != 4 || calls != 1) || !release && (count != 0 || calls != 0) {
		t.Fatal(release, count, calls)
	}
	view := prepared.View()
	view.Ref = "refs/tags/renamed"
	view.Workflow.Spec.Arguments.Parameters = nil
	if prepared.View().Ref != r.Ref || len(prepared.View().Workflow.Spec.Arguments.Parameters) == 0 {
		t.Fatal("admitted facts changed")
	}
}

func checkReleaseParameterValues(t *testing.T, w *wf.Workflow, facts admissioncontract.ReleaseFacts) int {
	t.Helper()
	count := 0
	for _, p := range w.Spec.Arguments.Parameters {
		if value, exists := facts.Parameters()[p.Name]; exists {
			count++
			if p.Value == nil || p.Value.String() != value {
				t.Fatal(p)
			}
		}
	}
	return count
}

func TestReleaseGlobalOutputsCannotChooseNamesDynamically(t *testing.T) {
	w := simpleWorkflow()
	w.Spec.Templates[0].Outputs.Parameters = []wf.Parameter{{Name: "output", GlobalName: "{{inputs.parameters.target}}", Value: wf.AnyStringPtr("forged")}}
	policy := submitPolicy()
	policy.Release = true
	r := requestFor(t, w)
	r.Trigger = model.TriggerTag
	_, err := prepareFixture(t.Context(), r, policy, identityHooks())
	requireCode(t, err, "reserved")
}

func TestOfflineReleaseReferencesNeverProduceFacts(t *testing.T) {
	w := simpleWorkflow()
	w.Spec.Templates[0].Container.Args = []string{"{{workflow.parameters.version}}"}
	if err := ValidateNativeSchema(t.Context(), w, admissioncontract.PrepareRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(w.Spec.Arguments.Parameters) != 0 {
		t.Fatal("offline validation produced facts")
	}
	if _, err := prepareFixture(t.Context(), requestFor(t, w), submitPolicy(), identityHooks()); err == nil {
		t.Fatal("unverified run gained release parameter")
	}
}

func TestOrdinaryBuildKeepsVersionArgumentsAndGlobalOutputs(t *testing.T) {
	for name := range (admissioncontract.ReleaseFacts{}).Parameters() {
		for _, dynamic := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "/literal", true: "/dynamic"}[dynamic], func(t *testing.T) {
				w := simpleWorkflow()
				w.Spec.Arguments.Parameters = []wf.Parameter{{Name: name, Value: wf.AnyStringPtr("authored")}}
				global := name
				if dynamic {
					global = "{{workflow.parameters." + name + "}}"
				}
				w.Spec.Templates[0].Outputs.Parameters = []wf.Parameter{{Name: "output", GlobalName: global, Value: wf.AnyStringPtr("authored-output")}}
				prepared, err := prepareFixture(t.Context(), requestFor(t, w), submitPolicy(), identityHooks())
				if err != nil {
					t.Fatal(err)
				}
				got := prepared.View().Workflow
				if got.Spec.Arguments.Parameters[0].Value.String() != "authored" || got.Spec.Templates[0].Outputs.Parameters[0].GlobalName != global {
					t.Fatal("ordinary parameters changed")
				}
			})
		}
	}
}

func TestBuildCheckCannotInventReleaseParameterValues(t *testing.T) {
	w := simpleWorkflow()
	w.Spec.Templates[0].Container.Args = []string{"{{workflow.parameters.version}}"}
	if err := ValidateNativeSchema(t.Context(), w, admissioncontract.PrepareRequest{}); err != nil {
		t.Fatal("standalone validation lost unknown-role placeholders", err)
	}
	if len(w.Spec.Arguments.Parameters) != 0 {
		t.Fatal("standalone validation changed the authored workflow")
	}
	for _, mode := range []admissioncontract.Mode{admissioncontract.ModeInspect, admissioncontract.ModeSubmit} {
		r := requestFor(t, w)
		r.Mode = mode
		r.IdentityRequest.Mode = mode
		if _, err := prepareFixture(t.Context(), r, submitPolicy(), identityHooks()); err == nil {
			t.Fatal("build gained an undeclared version", mode)
		}
	}
	r := requestFor(t, w)
	r.Mode = admissioncontract.ModeInspect
	r.IdentityRequest.Mode = r.Mode
	r.Trigger = model.TriggerTag
	policy := submitPolicy()
	policy.Release = true
	prepared, err := prepareFixture(t.Context(), r, policy, identityHooks())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range prepared.View().Workflow.Spec.Arguments.Parameters {
		if admissioncontract.IsReleaseParameter(p.Name) {
			t.Fatal("schema placeholder became run evidence")
		}
	}
}
