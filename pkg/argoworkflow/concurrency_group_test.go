package argoworkflow

import (
	"strings"
	"testing"
)

// groupedDocument is admissibleDocument declaring the retired concurrency
// group (and optionally limit) annotations.
func groupedDocument(annotations string) string {
	return strings.Replace(admissibleDocument, "kind: Workflow\n",
		"kind: Workflow\nmetadata:\n  annotations:\n"+annotations, 1)
}

// Since v0.20.0 the concurrency-group annotations are accepted and ignored,
// whatever their value: a document that still carries one keeps running, and
// a value the old grammar refused is no longer an admission error.
func TestAdmitIgnoresRetiredConcurrencyGroupAnnotations(t *testing.T) {
	for _, annotations := range []string{
		"    " + RetiredConcurrencyGroupAnnotation + ": terraform-state\n",
		"    " + RetiredConcurrencyGroupAnnotation + ": Terraform_State\n",
		"    " + RetiredConcurrencyGroupAnnotation + ": \"" + strings.Repeat("g", 64) + "\"\n",
		"    " + RetiredConcurrencyLimitAnnotation + ": \"0\"\n",
		"    " + RetiredConcurrencyGroupAnnotation + ": kvm\n    " + RetiredConcurrencyLimitAnnotation + ": \"two\"\n",
	} {
		mustAdmit(t, groupedDocument(annotations), Policy{})
	}
}
