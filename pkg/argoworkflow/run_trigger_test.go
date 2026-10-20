package argoworkflow

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/pkg/periapsis"
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

func TestDeclaredSizeForResolvesPerTriggerThenFallback(t *testing.T) {
	workflow := decodeAdmissible(t, map[string]string{
		SizeAnnotation:          "XL",
		SizeByTriggerAnnotation: " branch = M , promotion=XL",
	})
	for trigger, want := range map[RunTrigger]periapsis.Size{
		RunTriggerBranch:    periapsis.M,
		RunTriggerPromotion: periapsis.XL,
		// Not named: falls back to oberth.ci/size, not to M.
		RunTriggerSchedule: periapsis.XL,
	} {
		got, err := DeclaredSizeFor(workflow, trigger)
		if err != nil || got != want {
			t.Fatalf("%s size = %q, %v; want %q", trigger, got, err, want)
		}
	}

	// Neither annotation: every trigger is M, exactly as before.
	bare := decodeAdmissible(t, nil)
	for _, trigger := range []RunTrigger{RunTriggerBranch, RunTriggerPromotion, RunTriggerSchedule} {
		if got, err := DeclaredSizeFor(bare, trigger); err != nil || got != periapsis.M {
			t.Fatalf("undeclared %s size = %q, %v; want M", trigger, got, err)
		}
	}

	// Only the per-trigger annotation: unnamed triggers are M.
	only := decodeAdmissible(t, map[string]string{SizeByTriggerAnnotation: "promotion=L"})
	if got, err := DeclaredSizeFor(only, RunTriggerBranch); err != nil || got != periapsis.M {
		t.Fatalf("unnamed branch size = %q, %v; want M", got, err)
	}
	if got, err := DeclaredSizeFor(only, RunTriggerPromotion); err != nil || got != periapsis.L {
		t.Fatalf("promotion size = %q, %v; want L", got, err)
	}

	// A release document keys its one trigger as tag.
	release := decodeAdmissible(t, map[string]string{SizeAnnotation: "S", SizeByTriggerAnnotation: "tag=L"})
	if got, err := DeclaredSizeFor(release, RunTriggerTag); err != nil || got != periapsis.L {
		t.Fatalf("tag size = %q, %v; want L", got, err)
	}
}

func TestSizeByTriggerRefusesOutsideTheClosedVocabulary(t *testing.T) {
	for value, expect := range map[string]string{
		"branch=M,pr=XL":           `unknown trigger "pr"`,
		"Branch=M":                 `unknown trigger "Branch"`,
		"branch=XXL":               `unknown size "XXL"`,
		"branch=m":                 `unknown size "m"`,
		"branch=M,branch=XL":       "branch more than once",
		"branch=M,":                "is not trigger=size",
		"branch":                   "is not trigger=size",
		"=M":                       "is not trigger=size",
		"branch=M;promotion=XL":    `unknown size "M;promotion=XL"`,
		"branch:M":                 "is not trigger=size",
		"branch=M,promotion=XL=XL": `unknown size "XL=XL"`,
	} {
		workflow := decodeAdmissible(t, map[string]string{SizeByTriggerAnnotation: value})
		if _, err := DeclaredSizesByTrigger(workflow); err == nil || !strings.Contains(err.Error(), expect) {
			t.Fatalf("%q: error %v does not mention %q", value, err, expect)
		}
		// The document-agnostic gate `oberth validate` and server admission
		// both run refuses it too, and so does the scheduler's size read.
		if err := Admit(workflow, Policy{}); err == nil || !strings.Contains(err.Error(), expect) {
			t.Fatalf("%q: Admit error %v does not mention %q", value, err, expect)
		}
		if _, err := DeclaredSizeFor(workflow, RunTriggerBranch); err == nil {
			t.Fatalf("%q: scheduler size read accepted it", value)
		}
	}
	// A malformed fallback is refused by Admit as well, which it was not before.
	workflow := decodeAdmissible(t, map[string]string{SizeAnnotation: "huge"})
	if err := Admit(workflow, Policy{}); err == nil || !strings.Contains(err.Error(), `unknown size "huge"`) {
		t.Fatalf("Admit accepted oberth.ci/size: huge: %v", err)
	}
}

func TestSizeByTriggerRefusesTriggersTheDocumentNeverRuns(t *testing.T) {
	build := decodeAdmissible(t, map[string]string{SizeByTriggerAnnotation: "branch=M,tag=XL"})
	if err := AdmitSizeDeclarations(build, periapsis.TriggerCI); err == nil || !strings.Contains(err.Error(), "names tag") {
		t.Fatalf("build.yaml naming tag: %v", err)
	}
	// Even a branch run of that document is refused: half-obeying a
	// declaration the server knows to be wrong would hide the mistake.
	if _, err := DeclaredSizeFor(build, RunTriggerBranch); err == nil {
		t.Fatal("branch size read accepted a build.yaml that names tag")
	}
	release := decodeAdmissible(t, map[string]string{SizeByTriggerAnnotation: "promotion=XL"})
	if err := AdmitSizeDeclarations(release, periapsis.TriggerRelease); err == nil || !strings.Contains(err.Error(), "names promotion") {
		t.Fatalf("release.yaml naming promotion: %v", err)
	}
	if _, err := DeclaredSizeFor(release, RunTriggerTag); err == nil {
		t.Fatal("tag size read accepted a release.yaml that names promotion")
	}
	if _, err := DeclaredSizeFor(decodeAdmissible(t, nil), RunTrigger("push")); err == nil {
		t.Fatal("an unknown run trigger resolved a size")
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
