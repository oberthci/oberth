package argojob

import (
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// Delete with the after-deploy pipeline patch. Only the removed annotation
// distinguishes the deployed-server documents from current admission.
func TestCurrentOberthPipelinesTransitionalAdmission(t *testing.T) {
	for _, name := range []string{"build.yaml", "release.yaml"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile("../../.oberth/" + name)
			if err != nil {
				t.Fatal(err)
			}
			wf, err := argoworkflow.Decode(raw)
			if err != nil {
				t.Fatal(err)
			}
			err = argoworkflow.Admit(wf, argoworkflow.Policy{})
			if name == "build.yaml" {
				const removed = "oberth.ci/nonroot-templates"
				if wf.Annotations[removed] != "test-static-git-runtime" || err == nil || !strings.Contains(err.Error(), `metadata.annotations["`+removed+`"] is refused`) {
					t.Fatalf("unexpected transitional refusal: %v", err)
				}
				delete(wf.Annotations, removed)
			} else if err != nil {
				t.Fatal(err)
			}
			if err := argoworkflow.Admit(wf, argoworkflow.Policy{}); err != nil {
				t.Fatalf("additional pipeline drift: %v", err)
			}
		})
	}
}
