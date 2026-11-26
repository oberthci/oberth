package argojob

import (
	"errors"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/publicerr"
)

// Argo's retry panics when it cannot decompress nodes; the one call site turns
// that panic into a refusal instead of ending the server.
func TestNativeRetryPanicRefuses(t *testing.T) {
	t.Parallel()
	w := &wf.Workflow{ObjectMeta: meta.ObjectMeta{Name: "run-1"}, Status: wf.WorkflowStatus{Phase: wf.WorkflowFailed, CompressedNodes: "not compressed"}}
	_, _, err := formulateRetry(t.Context(), w)
	var p *publicerr.Error
	if !errors.As(err, &p) || p.Reason() != ReasonResumeStart {
		t.Fatal("panic not refused", err)
	}
}
