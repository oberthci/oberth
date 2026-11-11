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

// Removed annotations are refused with what replaced them, never ignored.
func TestAdmitRefusesRemovedAnnotations(t *testing.T) {
	for name, want := range map[string]string{
		"oberth.ci/concurrency-group": "native Argo mutex or semaphore",
		"oberth.ci/concurrency-limit": "native Argo mutex or semaphore",
		"oberth.ci/size":              "resources block",
		"oberth.ci/size-by-trigger":   "resources block",
	} {
		workflow, err := Decode([]byte(groupedDocument("    " + name + ": \"2\"\n")))
		if err != nil {
			t.Fatal(err)
		}
		err = Admit(workflow, Policy{})
		if err == nil || !strings.Contains(err.Error(), name+" is removed") || !strings.Contains(err.Error(), want) {
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
	if err := Admit(workflow, Policy{}); err == nil || !strings.Contains(err.Error(), "oberth.ci/exclusive is removed") || !strings.Contains(err.Error(), "semaphore") {
		t.Fatalf("exclusive: %v", err)
	}
}
