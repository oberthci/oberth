package app

import (
	"errors"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestScheduleExpressionMapKeys(t *testing.T) {
	t.Parallel()
	err := scheduleExpressions(wf.WorkflowSpec{PodMetadata: &wf.Metadata{Annotations: map[string]string{"{{workflow.name}}": "literal"}}})
	var refusal *Refusal
	if !errors.As(err, &refusal) || !publicerr.Permanent(err) || len(refusal.Problems) != 1 || refusal.Problems[0].Path != "spec.podMetadata.annotations.{{workflow.name}}" {
		t.Fatal("schedule substitution in a map key was not named", err)
	}
}
