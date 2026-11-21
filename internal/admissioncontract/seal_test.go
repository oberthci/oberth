package admissioncontract

import (
	"bytes"
	"encoding/json"
	"errors"
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/runtimeconfig"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func preparedFixture() PreparedView {
	return PreparedView{Workflow: &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "main", Templates: []wfv1.Template{{Name: "main"}}}},
		Repository: "forge/org/repo", RunID: "run", Ref: "main", ObjectSHA: "object", CommitSHA: "commit", Trigger: "branch",
		SelectedFile: ".oberth/build.yaml", FileBlob: "blob",
		SourceReceipt:  sourcecontract.Receipt{Version: 1, DocumentDigest: "authored"},
		SourceBindings: []sourcecontract.Binding{sourceBindingFixture()},
		Defaults:       runtimeconfig.WorkflowDefaultsSnapshot{WorkflowDefaults: json.RawMessage(`{"spec":{"b":2,"a":1}}`), Digest: "defaults"},
		Templates:      []TemplateSnapshot{{Namespace: "ns", Name: "shared", UID: "uid", ResourceVersion: "1", Spec: json.RawMessage(`{"z":[],"a":"é<&"}`)}},
		Identity:       IdentityEvidence{State: "verified", Pipeline: AccountEvidence{Name: "pipeline", UID: "uid"}, Resource: &AccountEvidence{Name: "resource", UID: "uid"}},
		Grants:         []GrantEvidence{{File: ".oberth/build.yaml", Step: "main", Path: "public/path", Audience: "aud", State: "verified"}},
		Plan:           Plan{Entries: []PlanEntry{{Path: "main", Name: "main", Template: "main"}}, Exact: true}, Findings: []Finding{{Code: "snapshot"}}}
}

func mustSeal(t *testing.T, view PreparedView) *PreparedAdmission {
	t.Helper()
	prepared, err := Seal(view)
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func TestSealDigestsBindExactlyTheCanonicalViews(t *testing.T) {
	prepared := mustSeal(t, preparedFixture())
	view := prepared.View()
	raw, err := prepared.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Contains(raw, []byte(`: `)) || !bytes.Contains(raw, []byte(`é<&`)) {
		t.Fatalf("noncanonical output: %s", raw)
	}
	if len(view.Digest) != 64 || strings.ToLower(view.Digest) != view.Digest {
		t.Fatal("wrong digest format")
	}
}

func TestEverySerializedViewFactIsBoundExceptDigestItself(t *testing.T) {
	base := preparedFixture()
	original := mustSeal(t, base).View()
	cases := []struct {
		name   string
		change func(*PreparedView)
	}{
		{"metadata", func(v *PreparedView) { v.Workflow.Name = "different" }},
		{"spec", func(v *PreparedView) { v.Workflow.Spec.Entrypoint = "different" }},
		{"repo", func(v *PreparedView) { v.Repository += "x" }},
		{"run", func(v *PreparedView) { v.RunID += "x" }}, {"ref", func(v *PreparedView) { v.Ref += "x" }},
		{"object", func(v *PreparedView) { v.ObjectSHA += "x" }}, {"commit", func(v *PreparedView) { v.CommitSHA += "x" }},
		{"trigger", func(v *PreparedView) { v.Trigger = "tag" }}, {"schedule", func(v *PreparedView) { v.ScheduleEntry = "entry" }},
		{"file", func(v *PreparedView) { v.SelectedFile += "x" }}, {"blob", func(v *PreparedView) { v.FileBlob += "x" }},
		{"receipt", func(v *PreparedView) { v.SourceReceipt.DocumentDigest += "x" }},
		{"binding", func(v *PreparedView) { v.SourceBindings[0].AuthorizationDigest = "other" }},
		{"defaults_bytes", func(v *PreparedView) { v.Defaults.WorkflowDefaults = []byte(`{"spec":{}}`) }},
		{"defaults_digest", func(v *PreparedView) { v.Defaults.Digest += "x" }},
		{"template_spec", func(v *PreparedView) { v.Templates[0].Spec = []byte(`{}`) }},
		{"template_uid", func(v *PreparedView) { v.Templates[0].UID += "x" }},
		{"identity_uid", func(v *PreparedView) { v.Identity.Pipeline.UID += "x" }},
		{"grant", func(v *PreparedView) { v.Grants[0].Audience += "x" }},
		{"plan", func(v *PreparedView) { v.Plan.Entries[0].Path += "x" }},
		{"finding", func(v *PreparedView) { v.Findings[0].Reason += "x" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			view := base.Clone()
			tc.change(&view)
			changed := mustSeal(t, view).View()
			if changed.Digest == original.Digest {
				t.Fatal("changed fact is unbound")
			}
			if (changed.SpecDigest != original.SpecDigest) != (tc.name == "spec") {
				t.Fatal("spec digest binds wrong fields")
			}
		})
	}
	base.Digest = "forged"
	base.SpecDigest = "forged"
	if got := mustSeal(t, base).View(); got.Digest != original.Digest || got.SpecDigest != original.SpecDigest {
		t.Fatal("caller controls digest")
	}
}

func TestSealCanonicalSnapshotsIgnoreObjectKeyOrderAndWhitespace(t *testing.T) {
	base := preparedFixture()
	original := mustSeal(t, base).View()
	base.Defaults.WorkflowDefaults = []byte(` { "spec" : { "a" : 1, "b" : 2 } } `)
	base.Templates[0].Spec = []byte(` { "a":"é<&", "z": [] } `)
	if got := mustSeal(t, base).View(); got.Digest != original.Digest {
		t.Fatal("raw JSON order changes digest")
	}
}

func TestCanonicalArrayOrderAndLargeNumbersArePreserved(t *testing.T) {
	raw, err := sourcecontract.CanonicalJSON(json.RawMessage(`{"z":9007199254740993,"a":[2,1]}`))
	if err != nil || string(raw) != `{"a":[2,1],"z":9007199254740993}` {
		t.Fatalf("%s %v", raw, err)
	}
	other, _ := sourcecontract.CanonicalJSON(json.RawMessage(`{"a":[1,2],"z":9007199254740993}`))
	if bytes.Equal(raw, other) {
		t.Fatal("array order was lost")
	}
}

func TestSealSortsTemplateIdentityAndPreservesOtherArrayOrder(t *testing.T) {
	base := preparedFixture()
	base.Templates = append(base.Templates, TemplateSnapshot{Namespace: "aa", Name: "first", Spec: []byte(`{}`)})
	first := mustSeal(t, base).View()
	base.Templates[0], base.Templates[1] = base.Templates[1], base.Templates[0]
	second := mustSeal(t, base).View()
	if first.Digest != second.Digest || first.Templates[0].Namespace != "aa" {
		t.Fatal("template sorting is unstable")
	}
}

func TestSealRefusesInvalidPreparedRepresentation(t *testing.T) {
	cases := []struct {
		name   string
		change func(*PreparedView)
	}{
		{"missing_workflow", func(v *PreparedView) { v.Workflow = nil }},
		{"invalid_snapshot", func(v *PreparedView) { v.Templates[0].Spec = []byte(`{`) }},
		{"trailing_snapshot", func(v *PreparedView) { v.Templates[0].Spec = []byte(`{} {}`) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			view := preparedFixture()
			tc.change(&view)
			prepared, err := Seal(view)
			var invalid *SealError
			if prepared != nil || !errors.As(errors.Join(errors.New("another"), err), &invalid) || invalid.Path == "" {
				t.Fatalf("%v %v", prepared, err)
			}
			for text, limit := range map[string]int{invalid.Reason(): 80, invalid.Remedy(): 120} {
				if problem := publicerr.CheckMessage(text, limit); problem != nil {
					t.Fatal(problem, text)
				}
			}
		})
	}
}

func TestZeroPreparedCannotProduceSealedJSON(t *testing.T) {
	var prepared PreparedAdmission
	_, err := json.Marshal(prepared)
	var invalid *SealError
	if !errors.As(err, &invalid) {
		t.Fatalf("zero value serialized as sealed: %v", err)
	}
	if !reflect.DeepEqual(prepared.View(), PreparedView{}) {
		t.Fatal("zero view grants evidence")
	}
}

func TestSnapshotObjectKeyPermutationsPreserveDigests(t *testing.T) {
	var want string
	for _, inner := range threeKeyObjects([]string{`"number":9007199254740993`, `"array":[2,1]`, `"text":"é<&"`}) {
		for _, outer := range threeKeyObjects([]string{`"nested":` + string(inner), `"empty":{}`, `"flag":true`}) {
			view := preparedFixture()
			view.Defaults.WorkflowDefaults = outer
			view.Templates[0].Spec = outer
			got := mustSeal(t, view).View().Digest
			if want == "" {
				want = got
			}
			if got != want {
				t.Fatalf("key permutation changes digest: %s", outer)
			}
		}
	}
}

func threeKeyObjects(members []string) []json.RawMessage {
	orders := [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	values := make([]json.RawMessage, 0, len(orders))
	for _, order := range orders {
		values = append(values, json.RawMessage("{"+members[order[0]]+","+members[order[1]]+","+members[order[2]]+"}"))
	}
	return values
}
