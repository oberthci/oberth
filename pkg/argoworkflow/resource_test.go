package argoworkflow

import (
	"encoding/base64"
	"encoding/json"
	"github.com/argoproj/argo-workflows/v4/util/logging"
	argotemplate "github.com/argoproj/argo-workflows/v4/util/template"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func resourceWorkflow(manifest string) (*wfv1.Workflow, admissionFixture) {
	wf := simpleWorkflow()
	wf.Spec.Templates[0] = wfv1.Template{Name: "main", Resource: &wfv1.ResourceTemplate{Action: "create", Manifest: manifest}}
	p := admissionFixture{Policy: Policy{ServerNamespace: "oberth", ArgoNamespace: "argo", Capability: &Capability{Namespace: "resources", ServiceAccount: "resources", ResourceKinds: []ResourceKind{{APIVersion: "example.test/v1", Kind: "Widget", Actions: []string{"create"}}}}}, PrepareRequest: admissioncontract.PrepareRequest{IdentityRequest: admissioncontract.IdentityRequest{Namespace: "ci"}}}
	return wf, p
}

// Covers: REQ-C4-003
func TestExactNativeCapabilitiesContract(t *testing.T) {
	t.Parallel()
	good := `{"apiVersion":"example.test/v1","kind":"Widget","metadata":{"name":"test","namespace":"resources"},"spec":{"annotations":{"custom":"value"}}}`
	wf, p := resourceWorkflow(good)
	if err := admitFixture(t.Context(), wf, p); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"ascii_kind":        strings.Replace(good, `"kind"`, `"Kind"`, 1),
		"unicode_kind":      strings.Replace(good, `"kind"`, `"Kind"`, 1),
		"namespace_variant": strings.Replace(good, `"namespace"`, `"nameſpace"`, 1),
		"alias_with_exact":  strings.Replace(good, `"kind":"Widget"`, `"kind":"Widget","Kind":"Widget"`, 1),
		"duplicate":         strings.Replace(good, `"kind":"Widget"`, `"kind":"Widget","kind":"Widget"`, 1),
		"wrong_namespace":   strings.Replace(good, `"namespace":"resources"`, `"namespace":"other"`, 1),
		"wrong_kind":        strings.Replace(good, "Widget", "Other", 1),
		"dynamic":           strings.Replace(good, "Widget", "{{inputs.parameters.kind}}", 1),
		"nonstring":         strings.Replace(good, `"namespace":"resources"`, `"namespace":1`, 1),
		"many":              good + "\n---\n" + good,
		"array":             "[{}]", "empty": "{}",
	}
	for name, manifest := range cases {
		t.Run(name, func(t *testing.T) {
			bad := wf.DeepCopy()
			bad.Spec.Templates[0].Resource.Manifest = manifest
			if err := admitFixture(t.Context(), bad, p); err == nil {
				t.Fatal("accepted invalid envelope")
			}
		})
	}
	for _, action := range []string{"apply", "get", "delete", "patch"} {
		bad := wf.DeepCopy()
		bad.Spec.Templates[0].Resource.Action = action
		requireCode(t, admitFixture(t.Context(), bad, p), "capability")
	}
	native := wf.DeepCopy()
	native.Spec.Templates[0].Resource.Manifest = strings.Replace(good, `"test"`, `"{{workflow.name}}"`, 1)
	if err := admitFixture(t.Context(), native, p); err != nil {
		t.Fatal(err)
	}
}
func TestResourceAccountDistinctAndNoCredentialCombination(t *testing.T) {
	t.Parallel()
	wf, p := resourceWorkflow(`apiVersion: example.test/v1
kind: Widget
metadata: {name: test, namespace: resources}`)
	r := requestFor(t, wf)
	if result, err := prepareFixture(t.Context(), r, p, identityHooks()); result != nil || err == nil {
		t.Fatal("resource has no granted account")
	}
	r.IdentityRequest.ResourceAccount = "resources"
	result, err := prepareFixture(t.Context(), r, p, identityHooks())
	if err != nil {
		t.Fatal(err)
	}
	step := result.View().Workflow.Spec.Templates[0]
	if step.ServiceAccountName != "resources" || step.Executor.ServiceAccountName != r.IdentityRequest.PipelineAccount {
		t.Fatal("resource or pipeline account differs")
	}
}

func TestInlineResourceUsesItsGrantedAccount(t *testing.T) {
	t.Parallel()
	wf, p := resourceWorkflow(`apiVersion: example.test/v1
kind: Widget
metadata: {name: test, namespace: resources}`)
	work := wf.Spec.Templates[0]
	wf.Spec.Templates[0] = wfv1.Template{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "resource", Inline: &work}}}}
	r := requestFor(t, wf)
	r.IdentityRequest.ResourceAccount = "resources"
	result, err := prepareFixture(t.Context(), r, p, identityHooks())
	if err != nil {
		t.Fatal(err)
	}
	step := result.View().Workflow.Spec.Templates[0].DAG.Tasks[0].Inline
	if step.ServiceAccountName != "resources" || step.Executor.ServiceAccountName != r.IdentityRequest.PipelineAccount {
		t.Fatal("inline resource or pipeline account differs")
	}
}

func TestResourceExpressionsStayOutsideAuthority(t *testing.T) {
	t.Parallel()
	good := `{"apiVersion":"example.test/v1","kind":"Widget","metadata":{"name":"test","namespace":"resources"},"spec":{"publicKey":"{{=sprig.b64enc(inputs.parameters['public-key'])}}"}}`
	wf, p := resourceWorkflow(good)
	if err := admitFixture(t.Context(), wf, p); err != nil {
		t.Fatal("spec expression refused", err)
	}
	raw := strings.Replace(good, "{{=sprig.b64enc(inputs.parameters['public-key'])}}", "{{inputs.parameters.public-key}}", 1)
	wf, p = resourceWorkflow(raw)
	if err := admitFixture(t.Context(), wf, p); err == nil {
		t.Fatal("unencoded data expression accepted")
	}
	assertResourceIdentityLiteral(t, good, "{{inputs.parameters.authority}}")
	assertResourceIdentityLiteral(t, good, "{{=sprig.b64enc(inputs.parameters['public-key'])}}")
}

func TestEncodedResourceDataCannotInjectAuthority(t *testing.T) {
	// The native logger constructor mutates its process-wide initialization buffer.
	data := "'\nmetadata: {namespace: oberth}\nspec: '"
	source := `{"apiVersion":"example.test/v1","kind":"Widget","metadata":{"name":"test","namespace":"resources"},"spec":{"data":"{{=sprig.b64enc(inputs.parameters['public-key'])}}"}}`
	replaced, err := argotemplate.Replace(logging.WithLogger(t.Context(), logging.NewTestLogger(logging.Error, logging.Text)), source, map[string]any{"inputs.parameters.public-key": data}, false)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal([]byte(replaced), &object); err != nil {
		t.Fatal(err)
	}
	if object["metadata"].(map[string]any)["namespace"] != "resources" || object["spec"].(map[string]any)["data"] != base64.StdEncoding.EncodeToString([]byte(data)) {
		t.Fatal("expression injected resource authority", object)
	}
}

func assertResourceIdentityLiteral(t *testing.T, good, expression string) {
	t.Helper()
	for _, literal := range []string{`"example.test/v1"`, `"Widget"`, `"test"`, `"resources"`} {
		wf, p := resourceWorkflow(strings.Replace(good, literal, `"`+expression+`"`, 1))
		if err := admitFixture(t.Context(), wf, p); err == nil {
			t.Fatal("runtime resource authority accepted", literal)
		}
	}
}

func TestResourceExpressionRequiresWholeDoubleQuotedValue(t *testing.T) {
	t.Parallel()
	expression := "{{=sprig.b64enc(inputs.parameters['data'])}}"
	for _, tc := range []struct {
		name, value string
		accepted    bool
	}{
		{"quoted", `"` + expression + `"`, true},
		{"unquoted", expression, false},
		{"single-quoted", "'" + strings.ReplaceAll(expression, "'", "''") + "'", false},
		{"prefix", `"prefix` + expression + `"`, false},
		{"suffix", `"` + expression + `suffix"`, false},
		{"block", "|-\n    " + expression + "\n", false},
		{"comment", `"ordinary" # "` + expression + `"`, false},
		{"key", `"ordinary"` + "\n  \"" + expression + "\": value", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest := "apiVersion: example.test/v1\nkind: Widget\nmetadata: {name: test, namespace: resources}\nspec:\n  data: " + tc.value + "\n"
			wf, policy := resourceWorkflow(manifest)
			if err := admitFixture(t.Context(), wf, policy); (err == nil) != tc.accepted {
				t.Fatal("expression scalar form", tc.name, err)
			}
		})
	}
}
