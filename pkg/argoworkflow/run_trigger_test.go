package argoworkflow

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/pkg/pipeline"
)

func decodeAdmissible(t *testing.T, annotations map[string]string) *wfv1.Workflow {
	t.Helper()
	workflow, err := Decode([]byte(admissibleDocument))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	workflow.Annotations = annotations
	return workflow
}

// Every run records size M; an invalid trigger is still refused.
func TestDeclaredSizeForIsM(t *testing.T) {
	for _, trigger := range RunTriggers() {
		if got, err := DeclaredSizeFor(decodeAdmissible(t, nil), trigger); err != nil || got != pipeline.M {
			t.Fatalf("%s = %s, %v", trigger, got, err)
		}
	}
	if _, err := DeclaredSizeFor(decodeAdmissible(t, nil), RunTrigger("push")); err == nil {
		t.Fatal("invalid trigger accepted")
	}
}

const triggerGuardedDocument = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  arguments:
    parameters:
    - name: other
      value: kept
    - name: oberth-trigger
      value: promotion
  templates:
    - name: main
      dag:
        tasks:
        - name: fast
          template: leaf
        - name: matrix
          template: leaf
          when: "'{{workflow.parameters.oberth-trigger}}' != 'branch'"
    - name: leaf
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`

func TestTriggerParameterDeclarationIsOnlyAPlainDefault(t *testing.T) {
	mustAdmit(t, triggerGuardedDocument, Policy{})
	const declared = "      value: promotion"
	for _, replacement := range []struct{ with, expect string }{
		{"      value: nightly", "server-owned"},
		{"      default: promotion", "server-owned"},
		{declared + "\n      enum: [promotion]", "server-owned"},
		{"      valueFrom:\n        supplied: {}", "server-owned"},
		{declared + "\n    - name: oberth-trigger\n      value: branch", "more than once"},
	} {
		refuse(t, strings.Replace(triggerGuardedDocument, declared, replacement.with, 1), Policy{}, replacement.expect)
	}
}

func TestSetTriggerParameterReplacesTheDeclaredDefault(t *testing.T) {
	workflow, err := Decode([]byte(triggerGuardedDocument))
	if err != nil {
		t.Fatal(err)
	}
	if err := SetTriggerParameter(workflow, RunTriggerBranch); err != nil {
		t.Fatal(err)
	}
	parameters := workflow.Spec.Arguments.Parameters
	if len(parameters) != 2 || parameters[0].Name != "other" || parameters[0].Value.String() != "kept" ||
		parameters[1].Name != TriggerParameter || parameters[1].Value.String() != "branch" {
		t.Fatalf("parameters after stamping = %+v", parameters)
	}

	// Referenced but undeclared: added, so the when guard resolves.
	undeclared := strings.Replace(triggerGuardedDocument,
		"    - name: oberth-trigger\n      value: promotion\n", "", 1)
	workflow, err = Decode([]byte(undeclared))
	if err != nil {
		t.Fatal(err)
	}
	if err := SetTriggerParameter(workflow, RunTriggerSchedule); err != nil {
		t.Fatal(err)
	}
	parameters = workflow.Spec.Arguments.Parameters
	if len(parameters) != 2 || parameters[1].Name != TriggerParameter || parameters[1].Value.String() != "schedule" {
		t.Fatalf("parameters after stamping undeclared = %+v", parameters)
	}

	// Never mentioned: the document is left exactly as it was.
	plain := decodeAdmissible(t, nil)
	if err := SetTriggerParameter(plain, RunTriggerPromotion); err != nil {
		t.Fatal(err)
	}
	if len(plain.Spec.Arguments.Parameters) != 0 {
		t.Fatalf("unused parameter was stamped: %+v", plain.Spec.Arguments.Parameters)
	}
	if err := SetTriggerParameter(plain, RunTrigger("push")); err == nil {
		t.Fatal("an unknown run trigger was stamped")
	}
}
