package specsize

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func TestSubmittedSpecOmitsMetadataAndStatus(t *testing.T) {
	const fixtureBytes = 512 << 10
	wf := &wfv1.Workflow{}
	before, err := Submitted(wf)
	if err != nil {
		t.Fatal(err)
	}
	wf.Annotations = map[string]string{"large": strings.Repeat("a", fixtureBytes)}
	wf.Status.Message = strings.Repeat("b", fixtureBytes)
	after, err := Submitted(wf)
	if err != nil || after != before {
		t.Fatal(before, after, err)
	}
	wf.Spec.Entrypoint = strings.Repeat("a", fixtureBytes)
	after, err = Submitted(wf)
	if err != nil || after <= fixtureBytes {
		t.Fatal(after, err)
	}
	if _, err := Submitted(nil); err == nil {
		t.Fatal("nil accepted")
	}
}
