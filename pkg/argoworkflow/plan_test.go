package argoworkflow

import (
	"strings"
	"testing"
)

// planDocument is the idiomatic two-level shape: an entrypoint DAG whose tasks
// are burns, where a burn is either a nested steps/dag template of several
// steps or a single container invoked directly.
const planDocument = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: ci
  templates:
  - name: ci
    dag:
      tasks:
      - name: setup
        template: setup
      - name: test
        template: test
        depends: setup
      - name: verify
        template: verify
        depends: setup
  - name: setup
    steps:
    - - name: tools-dir
        template: shell
    - - name: link-go
        template: shell
  - name: verify
    dag:
      tasks:
      - name: vet
        template: shell
      - name: lint
        template: shell
  - name: test
    container:
      image: "golang:1.26"
      command: ["/bin/true"]
  - name: shell
    container:
      image: "golang:1.26"
      command: ["/bin/true"]
`

func plannedKeys(t *testing.T, source string) []string {
	t.Helper()
	workflow, err := Decode([]byte(source))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	planned, err := PlannedSteps(workflow)
	if err != nil {
		t.Fatalf("PlannedSteps: %v", err)
	}
	keys := make([]string, 0, len(planned))
	for index, step := range planned {
		if step.Ordinal != index {
			t.Fatalf("ordinal %d at position %d: %+v", step.Ordinal, index, planned)
		}
		keys = append(keys, step.Burn+"/"+step.Step)
	}
	return keys
}

// TestPlannedStepsEnumeratesEveryDeclaredStepInDeclarationOrder is the whole
// premise: the complete inventory is knowable from the document, before any
// Pod exists, in an order that does not depend on what ran.
func TestPlannedStepsEnumeratesEveryDeclaredStepInDeclarationOrder(t *testing.T) {
	got := strings.Join(plannedKeys(t, planDocument), " ")
	want := "setup/tools-dir setup/link-go test/test verify/vet verify/lint"
	if got != want {
		t.Fatalf("planned steps =\n  %s\nwant\n  %s", got, want)
	}
}

// TestPlannedStepsNamesADirectPodAsItsOwnBurn pins the second half of the
// naming rule: a Pod the entrypoint invokes directly is a burn of one step,
// which is what the runtime derives for a Pod node bounded by the root.
func TestPlannedStepsNamesADirectPodAsItsOwnBurn(t *testing.T) {
	for _, key := range plannedKeys(t, planDocument) {
		if strings.HasPrefix(key, "test/") && key != "test/test" {
			t.Fatalf("direct container task named %q", key)
		}
	}
}

// TestPlannedStepsSkipsRuntimeFanOut proves the honest treatment of a
// construct whose step count is a runtime property: no row at all, rather than
// one row no observation could ever reconcile with the N pods Argo creates.
func TestPlannedStepsSkipsRuntimeFanOut(t *testing.T) {
	document := `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: ci
  templates:
  - name: ci
    dag:
      tasks:
      - name: fixed
        template: shell
      - name: fanned
        template: shell
        withItems: ["a", "b"]
      - name: parameterised
        template: shell
        withParam: "{{item}}"
  - name: shell
    container:
      image: "golang:1.26"
      command: ["/bin/true"]
`
	got := strings.Join(plannedKeys(t, document), " ")
	if got != "fixed/fixed" {
		t.Fatalf("planned steps = %q, want only the non-expanding task", got)
	}
}

// TestPlannedStepsRefusesATemplateCycle keeps a repository-authored document
// from making the walk unbounded. Admission proves every reference resolves,
// not that the graph is acyclic.
func TestPlannedStepsRefusesATemplateCycle(t *testing.T) {
	document := `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: ci
  templates:
  - name: ci
    dag:
      tasks:
      - name: loop
        template: loop
  - name: loop
    dag:
      tasks:
      - name: again
        template: ci
`
	workflow, err := Decode([]byte(document))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, err := PlannedSteps(workflow); err == nil {
		t.Fatal("a self-reachable template graph enumerated without error")
	}
}

// TestPlannedStepsEnumeratesInlineTemplates covers the second recursive
// position in the schema, which admission also walks.
func TestPlannedStepsEnumeratesInlineTemplates(t *testing.T) {
	document := `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: ci
  templates:
  - name: ci
    dag:
      tasks:
      - name: inline-burn
        inline:
          dag:
            tasks:
            - name: inline-step
              inline:
                container:
                  image: "golang:1.26"
                  command: ["/bin/true"]
`
	got := strings.Join(plannedKeys(t, document), " ")
	if got != "inline-burn/inline-step" {
		t.Fatalf("planned steps = %q", got)
	}
}

// TestPlannedStepsLeavesAPodEntrypointUnplanned documents the one shape the
// plan deliberately cannot name: Argo names that single node after the
// Workflow object, which the document does not know, so a planned row would
// name a step the run never reports.
func TestPlannedStepsLeavesAPodEntrypointUnplanned(t *testing.T) {
	document := `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: only
  templates:
  - name: only
    container:
      image: "golang:1.26"
      command: ["/bin/true"]
`
	if got := plannedKeys(t, document); len(got) != 0 {
		t.Fatalf("planned steps = %v, want none", got)
	}
}

func TestPlannedStepsRequiresAResolvableEntrypoint(t *testing.T) {
	workflow, err := Decode([]byte(planDocument))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	workflow.Spec.Entrypoint = "absent"
	if _, err := PlannedSteps(workflow); err == nil {
		t.Fatal("an entrypoint naming no template enumerated without error")
	}
	workflow.Spec.Entrypoint = ""
	if _, err := PlannedSteps(workflow); err == nil {
		t.Fatal("a missing entrypoint enumerated without error")
	}
	if _, err := PlannedSteps(nil); err == nil {
		t.Fatal("a nil Workflow enumerated without error")
	}
}

// TestBurnAndStepIsTheOneRule is the guard that keeps the static enumeration
// and the runtime projection from drifting: both call this, so a change here
// changes both or neither.
func TestBurnAndStepIsTheOneRule(t *testing.T) {
	if burn, step := BurnAndStep("", "solo"); burn != "solo" || step != "solo" {
		t.Fatalf("unbounded pod = %q/%q, want solo/solo", burn, step)
	}
	if burn, step := BurnAndStep("setup", "tools-dir"); burn != "setup" || step != "tools-dir" {
		t.Fatalf("bounded pod = %q/%q, want setup/tools-dir", burn, step)
	}
	// The characters the progress protocol and the run-log index cannot carry.
	if burn, step := BurnAndStep("  se[t]up  ", "a\nb"); burn != "se-t-up" || step != "a-b" {
		t.Fatalf("normalised = %q/%q", burn, step)
	}
}

// TestPlannedStepsRejectsDuplicateStepKeys verifies that a pipeline producing
// duplicate (burn, step) keys is rejected at plan time because the durable
// result model cannot represent them (deduplicateRetries collapses duplicates).
func TestPlannedStepsRejectsDuplicateStepKeys(t *testing.T) {
	// A steps template that runs the same named step in two sequential
	// groups. Each invocation normalizes to the same (burn, step) key
	// since the enclosing context is the same.
	document := `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: ci
  templates:
  - name: ci
    dag:
      tasks:
      - name: build
        template: build-burn
  - name: build-burn
    steps:
    - - name: compile
        template: shell
    - - name: compile
        template: shell
  - name: shell
    container:
      image: "golang:1.26"
      command: ["/bin/true"]
`
	workflow, err := Decode([]byte(document))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	_, err = PlannedSteps(workflow)
	if err == nil {
		t.Fatal("pipeline with duplicate step keys should be rejected at plan time")
	}
	if !strings.Contains(err.Error(), "duplicate step key") {
		t.Fatalf("error should mention duplicate step key, got: %v", err)
	}
}
