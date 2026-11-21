package admissioncontract

import (
	"encoding/json"
	"errors"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func TestSealRefusesMalformedNativeItemWithoutPanic(t *testing.T) {
	view := preparedFixture()
	view.Workflow.Spec.Templates[0].DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "item", WithItems: []wfv1.Item{{Value: json.RawMessage("{")}}}}}
	prepared, err := Seal(view)
	var refusal *SealError
	if prepared != nil || !errors.As(err, &refusal) || refusal.Path != "workflow.spec" {
		t.Fatalf("got %v, %v", prepared, err)
	}
}
