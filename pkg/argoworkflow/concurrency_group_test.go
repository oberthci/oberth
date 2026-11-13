package argoworkflow

import (
	"strings"
	"testing"
)

// groupedDocument is admissibleDocument declaring workflow annotations.
func groupedDocument(annotations string) string {
	return strings.Replace(admissibleDocument, "kind: Workflow\n",
		"kind: Workflow\nmetadata:\n  annotations:\n"+annotations, 1)
}

// Unknown ownership keys use the same general refusal, without retired-name messages.
func TestAdmitRefusesRemovedAnnotations(t *testing.T) {
	for _, name := range []string{"oberth.ci/concurrency-group", "oberth.ci/size", "oberth.ci/go-proxy"} {
		workflow, err := Decode([]byte(groupedDocument("    " + name + ": \"2\"\n")))
		if err != nil {
			t.Fatal(err)
		}
		err = Admit(workflow, Policy{})
		if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "assigned by Oberth") || strings.Contains(err.Error(), "is removed") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	mustAdmit(t, groupedDocument("    example.com/tier: L\n"), Policy{})
}

func TestAdmitRefusesRemovedExclusiveTemplateAnnotation(t *testing.T) {
	document := strings.Replace(admissibleDocument, "    - name: main\n", "    - name: main\n      metadata:\n        annotations:\n          oberth.ci/exclusive: kvm\n", 1)
	workflow, err := Decode([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	if err := Admit(workflow, Policy{}); err == nil || !strings.Contains(err.Error(), "annotations is assigned by Oberth") || strings.Contains(err.Error(), "is removed") {
		t.Fatalf("exclusive: %v", err)
	}
}
