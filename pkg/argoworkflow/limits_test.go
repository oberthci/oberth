package argoworkflow

import (
	"context"
	"fmt"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
)

func TestDocumentExactByteBoundary(t *testing.T) {
	t.Parallel()
	raw := mustDocument(t, simpleWorkflow())
	padded := append(raw, []byte(strings.Repeat(" ", admissioncontract.MaxDocumentBytes-len(raw)))...)
	if _, err := Decode(padded); err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(append(padded, ' ')); err == nil {
		t.Fatal("over-limit document")
	} else {
		requireLimit(t, err, "document_size", "1 MiB")
	}
}
func TestSecretPathAndCapabilityKindBounds(t *testing.T) {
	t.Parallel()
	wf := simpleWorkflow()
	p := admissionFixture{Policy: Policy{}, PrepareRequest: admissioncontract.PrepareRequest{Mode: admissioncontract.ModeInspect, Repository: "forge/org/repo", SelectedFile: ".oberth/build.yaml"}}
	p.OfflineValidation = true
	paths := []string{}
	for i := range admissioncontract.MaxSecretPaths {
		paths = append(paths, fmt.Sprintf("public/key%d", i))
	}
	declareSecretPaths(&wf.Spec.Templates[0], strings.Join(paths, ","))
	if err := admitFixture(t.Context(), wf, p); err != nil {
		t.Fatal(err)
	}
	declareSecretPaths(&wf.Spec.Templates[0], strings.Join(paths, ",")+",public/overflow")
	requireLimit(t, admitFixture(t.Context(), wf, p), "secret_count", "32")
	capability := &Capability{Namespace: "resources", ServiceAccount: "resources"}
	for i := range admissioncontract.MaxCapabilityKinds {
		capability.ResourceKinds = append(capability.ResourceKinds, ResourceKind{APIVersion: "example.test/v1", Kind: fmt.Sprintf("Kind%d", i), Actions: []string{"get"}})
	}
	p.IdentityRequest.Namespace = "ci"
	p.Policy = Policy{ServerNamespace: "oberth", ArgoNamespace: "argo", Capability: capability}
	wf.Spec.Templates[0].Metadata.Annotations = nil
	if err := admitFixture(t.Context(), wf, p); err != nil {
		t.Fatal("valid capability refused")
	}
	capability.ResourceKinds = append(capability.ResourceKinds, ResourceKind{APIVersion: "example.test/v1", Kind: "Overflow", Actions: []string{"get"}})
	if err := admitFixture(t.Context(), wf, p); err == nil {
		t.Fatal("over-limit capability")
	}
}
func TestReferenceReadsStopAtTheDepthBoundary(t *testing.T) {
	t.Parallel()
	wf := sharedConsumer("r0", "work")
	reads := 0
	p := admissionFixture{Policy: Policy{TemplateGetter: func(_ context.Context, name string) (*wfv1.WorkflowTemplate, error) {
		reads++
		obj := sharedObject(name)
		obj.Spec.Templates[0] = wfv1.Template{Name: "work", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "next", TemplateRef: &wfv1.TemplateRef{Name: fmt.Sprintf("r%d", reads), Template: "work"}}}}}
		return obj, nil
	}}, PrepareRequest: admissioncontract.PrepareRequest{IdentityRequest: admissioncontract.IdentityRequest{Namespace: "ci"}},
	}
	requireCode(t, admitFixture(t.Context(), wf, p), "depth")
	if reads != admissioncontract.MaxNestingDepth {
		t.Fatalf("reads=%d", reads)
	}
}
