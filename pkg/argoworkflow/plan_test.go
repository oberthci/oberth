package argoworkflow

import (
	"fmt"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
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
		keys = append(keys, step.Stage+"/"+step.Step)
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
func TestPlannedStepsNamesADirectPodAsItsOwnStage(t *testing.T) {
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
func TestPlannedStepsAcceptsATemplateCycle(t *testing.T) {
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
	if _, err := PlannedSteps(workflow); err != nil {
		t.Fatalf("native recursion was refused: %v", err)
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
func TestStageAndStepIsTheOneRule(t *testing.T) {
	if stage, step := StageAndStep("", "solo"); stage != "solo" || step != "solo" {
		t.Fatalf("unbounded pod = %q/%q, want solo/solo", stage, step)
	}
	if stage, step := StageAndStep("setup", "tools-dir"); stage != "setup" || step != "tools-dir" {
		t.Fatalf("bounded pod = %q/%q, want setup/tools-dir", stage, step)
	}
	// The characters the progress protocol and the run-log index cannot carry.
	if stage, step := StageAndStep("  se[t]up  ", "a\nb"); stage != "se-t-up" || step != "a-b" {
		t.Fatalf("normalised = %q/%q", stage, step)
	}
}

// Repeated display names remain valid because runtime rows use node IDs.
// The declaration walk reads each branch without collapsing it.
func TestPlannedStepsAllowsDuplicateDisplayKeys(t *testing.T) {
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
	planned, err := PlannedSteps(workflow)
	if err != nil || len(planned) != 2 {
		t.Fatalf("native repeated display names refused: %+v %v", planned, err)
	}
}

// secretsDocument nests two credential-chain leaves under one burn beside
// plain steps, including a template that merely invokes the oberth binary.
const secretsDocument = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: ci
  templates:
  - name: ci
    dag:
      tasks:
      - name: setup
        template: shell
      - name: publish
        template: publish
        depends: setup
      - name: version
        template: version
  - name: publish
    steps:
    - - name: fetch
        template: fetch
    - - name: push
        template: push
  - name: shell
    container:
      image: "golang:1.26"
      command: ["/bin/true"]
  - name: fetch
    container:
      image: "golang:1.26"
      command: ["/run/oberth/bin/oberth"]
      args: ["secretstore", "exec", "--path", "kv/ci/token", "--", "/bin/true"]
  - name: push
    script:
      image: "golang:1.26"
      command: ["oberth", "secretstore", "exec", "--path", "kv/ci/token", "--", "/bin/true"]
      source: "true"
  - name: version
    container:
      image: "golang:1.26"
      command: ["oberth", "version"]
`

// TestPlannedStepsMarksTheStepsThatRunACredentialChain pins the per-step
// secret flag to the one rule the credential mount injection applies: the
// template's main command is a recognised credential chain. Nothing else in
// the document -- not the workflow annotation, not a sibling step -- marks
// a step.
func TestPlannedStepsMarksTheStepsThatRunACredentialChain(t *testing.T) {
	workflow, err := Decode([]byte(secretsDocument))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	planned, err := PlannedSteps(workflow)
	if err != nil {
		t.Fatalf("PlannedSteps: %v", err)
	}
	got := make(map[string]bool, len(planned))
	for _, step := range planned {
		got[step.Stage+"/"+step.Step] = step.Secrets
	}
	want := map[string]bool{
		"setup/setup":     false,
		"publish/fetch":   true,
		"publish/push":    true,
		"version/version": false,
	}
	if len(got) != len(want) {
		t.Fatalf("planned %v, want keys %v", got, want)
	}
	for key, secrets := range want {
		if actual, ok := got[key]; !ok || actual != secrets {
			t.Errorf("%s secrets = %v (present %v), want %v", key, actual, ok, secrets)
		}
	}
}

func TestUsesSecrets(t *testing.T) {
	container := func(command []string, args ...string) *wfv1.Template {
		return &wfv1.Template{Container: &corev1.Container{Command: command, Args: args}}
	}
	script := func(command []string, args ...string) *wfv1.Template {
		return &wfv1.Template{Script: &wfv1.ScriptTemplate{Container: corev1.Container{Command: command, Args: args}}}
	}
	cases := []struct {
		name     string
		template *wfv1.Template
		want     bool
	}{
		{"envconsul receives no credential token", container([]string{"envconsul", "-config", "x.hcl"}), false},
		{"envconsul by path receives no credential token", script([]string{"/usr/local/bin/envconsul"}), false},
		{"secretstore exec in args", container([]string{"/run/oberth/bin/oberth"}, "secretstore", "exec", "--path", "kv/a"), true},
		{"retired materialize gets no credential token", script([]string{"oberth", "secretstore", "materialize"}), false},
		{"oberth without secretstore", container([]string{"oberth", "version"}), false},
		{"secretstore under another binary", container([]string{"sh"}, "secretstore", "exec"), false},
		{"secretstore subcommand alone", container([]string{"oberth"}, "secretstore"), false},
		{"retired observer does not unwrap", container([]string{"/run/oberth/usage/oberth", "internal-step-exec", "--", "oberth", "secretstore", "exec"}), false},
		{"plain shell", container([]string{"/bin/sh", "-c", "true"}), false},
		{"no pod", &wfv1.Template{}, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := UsesSecrets(testCase.template); got != testCase.want {
				t.Fatalf("UsesSecrets = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestPlannedStepsCredentialedIncludesReleaseWIF(t *testing.T) {
	workflow, err := Decode([]byte(planDocument))
	if err != nil {
		t.Fatal(err)
	}
	for i := range workflow.Spec.Templates {
		if workflow.Spec.Templates[i].Name == "test" {
			workflow.Spec.Templates[i].Metadata.Annotations = map[string]string{ReleaseWIFRoleAnnotation: "image-writer"}
		}
	}
	planned, err := PlannedSteps(workflow)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, step := range planned {
		if step.Stage == "test" && step.Step == "test" {
			found = true
			if !step.Credentialed || step.Secrets {
				t.Fatalf("WIF plan was not credentialed independently of secret store: %+v", step)
			}
		}
	}
	if !found {
		t.Fatal("missing WIF leaf")
	}
}

func TestPlannedStepsHasNoDisplayRowCap(t *testing.T) {
	workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "entry", Templates: []wfv1.Template{
		{Name: "entry", DAG: &wfv1.DAGTemplate{}},
		{Name: "leaf", Container: &corev1.Container{Image: "example:tag"}},
	}}}
	for i := range 2000 {
		workflow.Spec.Templates[0].DAG.Tasks = append(workflow.Spec.Templates[0].DAG.Tasks, wfv1.DAGTask{Name: fmt.Sprintf("task-%d", i), Template: "leaf"})
	}
	planned, err := PlannedSteps(workflow)
	if err != nil || len(planned) != 2000 {
		t.Fatalf("native fan-out was capped: rows=%d error=%v", len(planned), err)
	}
}
