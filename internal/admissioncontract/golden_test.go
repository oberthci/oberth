package admissioncontract

import (
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

// Fixed SHA-256 vectors use the hand-assembled native spec and prepared JSON.
// They do not derive expected values through the encoder under test.
func TestPreparedDigestsMatchFixedGoldenValues(t *testing.T) {
	view := PreparedView{Workflow: &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "main", Templates: []wfv1.Template{{Name: "main"}}}}}
	got := mustSeal(t, view).View()
	const wantSpec = "b36db1fce6020f5715c2e10486d57756cae7c298e9299856d15572f6fb3aacde"
	const wantView = "2c470fc751eaedee6944800e354c7c213af708f324f87fd425999a6281730a72"
	if got.SpecDigest != wantSpec || got.Digest != wantView {
		t.Fatalf("digests = %s, %s; want %s, %s", got.SpecDigest, got.Digest, wantSpec, wantView)
	}
}
