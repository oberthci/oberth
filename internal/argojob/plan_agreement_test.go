package argojob

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// The plan Oberth seeds at submission and the step list it records while a run
// executes are two derivations of the same thing: one from the document, one
// from Argo's node tree. If they disagree, a run seeds one set of rows and then
// records a second, disjoint set beside them -- the step count grows, the plan
// never resolves, and the durable record is worse than the gap it replaced.
//
// These tests hold the two together against the node names Argo really writes.

// stepsOrDagSeparator and the naming below are transcribed from
// argo-workflows/workflow/controller/operator.go (initializeNode), dag.go and
// steps.go. It is a deliberate second copy for the same reason
// upstreamGeneratePodName in this package is: a fixture that agrees with
// Oberth's own derivation because it was written from it proves nothing.
var stepsOrDagSeparator = regexp.MustCompile(`^(\[\d+\])?\.`)

// simulateArgoNodes builds the node tree Argo's controller writes for a
// Workflow that ran to completion.
//
//   - a DAG task's node is "<boundary>.<task>", bounded by the template
//     invocation that encloses it (dag.go: dagTaskNodeName)
//   - a steps template's Pod is "<boundary>[<group>].<step>", bounded by the
//     Steps node itself, not by the step group (steps.go: stepsCtx.boundaryID
//     is the Steps node ID)
//   - DisplayName is the node name with its boundary's name and the separator
//     trimmed (operator.go: initializeNode)
func simulateArgoNodes(t *testing.T, workflow *wfv1.Workflow) wfv1.Nodes {
	t.Helper()
	templates := map[string]*wfv1.Template{}
	for index := range workflow.Spec.Templates {
		templates[workflow.Spec.Templates[index].Name] = &workflow.Spec.Templates[index]
	}
	entrypoint, ok := templates[workflow.Spec.Entrypoint]
	if !ok {
		t.Fatalf("entrypoint %q names no template", workflow.Spec.Entrypoint)
	}
	nodes := wfv1.Nodes{}
	clock := time.Unix(1_700_000_000, 0).UTC()
	tick := func() metav1.Time {
		clock = clock.Add(time.Second)
		return metav1.NewTime(clock)
	}
	add := func(name, boundaryName, templateName string, kind wfv1.NodeType) {
		display := name
		if boundaryName != "" {
			display = stepsOrDagSeparator.ReplaceAllString(strings.TrimPrefix(name, boundaryName), "")
		}
		started := tick()
		nodes[name] = wfv1.NodeStatus{
			ID: name, Name: name, DisplayName: display, TemplateName: templateName,
			Type: kind, BoundaryID: boundaryName, Phase: wfv1.NodeSucceeded,
			StartedAt: started, FinishedAt: started,
		}
	}
	kindOf := func(template *wfv1.Template) wfv1.NodeType {
		switch {
		case template.DAG != nil:
			return wfv1.NodeTypeDAG
		case len(template.Steps) != 0:
			return wfv1.NodeTypeSteps
		default:
			return wfv1.NodeTypePod
		}
	}
	add(workflow.Name, "", workflow.Spec.Entrypoint, kindOf(entrypoint))

	var walk func(template *wfv1.Template, nodeName string, depth int)
	invoke := func(name, boundaryName, templateName string, target *wfv1.Template, depth int) {
		add(name, boundaryName, templateName, kindOf(target))
		if target.DAG != nil || len(target.Steps) != 0 {
			walk(target, name, depth+1)
		}
	}
	resolve := func(templateName string, inline *wfv1.Template) *wfv1.Template {
		if inline != nil {
			return inline
		}
		return templates[templateName]
	}
	walk = func(template *wfv1.Template, nodeName string, depth int) {
		if depth > 16 {
			t.Fatalf("simulated node tree nests deeper than the fixture supports")
		}
		if template.DAG != nil {
			for _, task := range template.DAG.Tasks {
				if len(task.WithItems) != 0 || task.WithParam != "" || task.WithSequence != nil {
					t.Fatalf("task %q fans out at runtime; this simulation cannot model it", task.Name)
				}
				target := resolve(task.Template, task.Inline)
				if target == nil {
					t.Fatalf("task %q references template %q, which the document does not declare", task.Name, task.Template)
				}
				invoke(nodeName+"."+task.Name, nodeName, task.Template, target, depth)
			}
		}
		for group := range template.Steps {
			groupName := fmt.Sprintf("%s[%d]", nodeName, group)
			add(groupName, nodeName, "", wfv1.NodeTypeStepGroup)
			for _, step := range template.Steps[group].Steps {
				if len(step.WithItems) != 0 || step.WithParam != "" || step.WithSequence != nil {
					t.Fatalf("step %q fans out at runtime; this simulation cannot model it", step.Name)
				}
				target := resolve(step.Template, step.Inline)
				if target == nil {
					t.Fatalf("step %q references template %q, which the document does not declare", step.Name, step.Template)
				}
				invoke(groupName+"."+step.Name, nodeName, step.Template, target, depth)
			}
		}
	}
	walk(entrypoint, workflow.Name, 0)
	return nodes
}

func plannedKeys(t *testing.T, workflow *wfv1.Workflow) []string {
	t.Helper()
	planned, err := argoworkflow.PlannedSteps(workflow)
	if err != nil {
		t.Fatalf("PlannedSteps: %v", err)
	}
	keys := make([]string, 0, len(planned))
	for _, step := range planned {
		keys = append(keys, step.Burn+"/"+step.Step)
	}
	sort.Strings(keys)
	return keys
}

func observedKeys(t *testing.T, workflow *wfv1.Workflow) []string {
	t.Helper()
	executed := workflow.DeepCopy()
	executed.Status = wfv1.WorkflowStatus{Phase: wfv1.WorkflowSucceeded, Nodes: simulateArgoNodes(t, executed)}
	results := StepResults(executed)
	keys := make([]string, 0, len(results))
	for _, result := range results {
		keys = append(keys, result.Burn+"/"+result.Step)
	}
	sort.Strings(keys)
	return keys
}

// TestPlannedStepsAgreeWithObservedStepsForOberthsOwnPipelines is the test that
// keeps the two derivations honest, against the first two production documents
// written in this format.
func TestPlannedStepsAgreeWithObservedStepsForOberthsOwnPipelines(t *testing.T) {
	for _, entry := range []struct {
		file    string
		trigger periapsis.Trigger
	}{
		{"build.yaml", periapsis.TriggerCI},
		{"release.yaml", periapsis.TriggerRelease},
	} {
		t.Run(entry.file, func(t *testing.T) {
			approved := map[string]bool{}
			if entry.trigger == periapsis.TriggerRelease {
				approved = oberthReleaseApprovedSecrets()
			}
			built, err := Build(oberthConfig(), Request{
				RunID: "run-abc123", Name: "oberth-oberth-run-abc1-aabbccddeeff",
				Repo: "oberth", UpstreamOrg: "skipops",
				Ref: "refs/heads/main", SHA: testSHA, Trigger: entry.trigger,
				Source:          loadPipeline(t, entry.file),
				ApprovedSecrets: approved,
			})
			if err != nil {
				t.Fatalf("build %s: %v", entry.file, err)
			}
			planned, observed := plannedKeys(t, built), observedKeys(t, built)
			if len(planned) == 0 {
				t.Fatal("the pipeline planned no steps at all")
			}
			if strings.Join(planned, "\n") != strings.Join(observed, "\n") {
				t.Fatalf("plan and observation disagree\nplanned:\n  %s\nobserved:\n  %s",
					strings.Join(planned, "\n  "), strings.Join(observed, "\n  "))
			}
		})
	}
}

// TestPlannedStepsAgreeAcrossNestedShapes covers the shapes Oberth's own
// documents do not currently use: a steps burn, a dag burn, a bare container
// burn, and a dag nested two levels deep.
func TestPlannedStepsAgreeAcrossNestedShapes(t *testing.T) {
	built, err := Build(testConfig(), testRequest(periapsis.TriggerCI, `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: ci
  activeDeadlineSeconds: 600
  templates:
  - name: ci
    dag:
      tasks:
      - name: steps-burn
        template: steps-burn
      - name: dag-burn
        template: dag-burn
      - name: bare
        template: shell
  - name: steps-burn
    steps:
    - - name: first
        template: shell
    - - name: second
        template: shell
  - name: dag-burn
    dag:
      tasks:
      - name: inner
        template: shell
      - name: deeper
        template: deep-burn
  - name: deep-burn
    dag:
      tasks:
      - name: deepest
        template: shell
  - name: shell
    container:
      image: "golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
      command: ["/bin/true"]
`))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	planned, observed := plannedKeys(t, built), observedKeys(t, built)
	if strings.Join(planned, "\n") != strings.Join(observed, "\n") {
		t.Fatalf("plan and observation disagree\nplanned:\n  %s\nobserved:\n  %s",
			strings.Join(planned, "\n  "), strings.Join(observed, "\n  "))
	}
	// A Pod two levels down reports the nearest enclosing invocation as its
	// burn, not the outermost one -- and the burn is that invocation's *task*
	// name ("deeper"), not the name of the template it calls ("deep-burn").
	// Argo's DisplayName is the node name with its boundary trimmed, so the
	// task name is what the runtime sees; a plan that used template names
	// would seed rows no observation could ever reconcile.
	want := "bare/bare dag-burn/inner deeper/deepest steps-burn/first steps-burn/second"
	if got := strings.Join(planned, " "); got != want {
		t.Fatalf("planned steps = %q, want %q", got, want)
	}
}
