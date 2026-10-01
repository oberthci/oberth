package argojob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// budgetTestWorkflow returns a terminal workflow with two burns, each
// containing one step pod. The steps have distinct start times so
// StepResults produces a deterministic order: build/compile then test/unit.
// Node IDs and DisplayNames follow the same conventions as nestedWorkflow().
func budgetTestWorkflow() *wfv1.Workflow {
	started1 := metav1.NewTime(time.Unix(1000, 0).UTC())
	finished1 := metav1.NewTime(time.Unix(1005, 0).UTC())
	started2 := metav1.NewTime(time.Unix(1006, 0).UTC())
	finished2 := metav1.NewTime(time.Unix(1010, 0).UTC())
	workflow := &wfv1.Workflow{}
	workflow.Name = "wf"
	workflow.Namespace = testNamespace
	workflow.Annotations = map[string]string{
		runIDAnnotation:          "run-budget",
		identityAnnotation:       "digest",
		PodNameVersionAnnotation: podNameVersionV2,
	}
	workflow.Spec = wfv1.WorkflowSpec{
		Entrypoint: "ci",
		Templates: []wfv1.Template{
			{Name: "ci", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "build", Template: "build"},
				{Name: "test", Template: "test"},
			}}},
			{Name: "build", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "compile", Template: "compile"},
			}}},
			{Name: "test", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "unit", Template: "unit"},
			}}},
			{Name: "compile", Container: &corev1.Container{Image: "golang:1.26"}},
			{Name: "unit", Container: &corev1.Container{Image: "golang:1.26"}},
		},
	}
	workflow.Status = wfv1.WorkflowStatus{
		Phase: wfv1.WorkflowSucceeded,
		Nodes: wfv1.Nodes{
			"root": {ID: "root", Name: "wf", DisplayName: "wf",
				Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeSucceeded},
			"build-burn": {ID: "build-burn", Name: "wf.build", DisplayName: "build",
				Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeSucceeded, BoundaryID: "root"},
			"test-burn": {ID: "test-burn", Name: "wf.test", DisplayName: "test",
				Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeSucceeded, BoundaryID: "root"},
			"compile-pod": {ID: "compile-pod", Name: "wf.build.compile", DisplayName: "compile",
				TemplateName: "compile", Type: wfv1.NodeTypePod, Phase: wfv1.NodeSucceeded,
				BoundaryID: "build-burn", StartedAt: started1, FinishedAt: finished1},
			"unit-pod": {ID: "unit-pod", Name: "wf.test.unit", DisplayName: "unit",
				TemplateName: "unit", Type: wfv1.NodeTypePod, Phase: wfv1.NodeSucceeded,
				BoundaryID: "test-burn", StartedAt: started2, FinishedAt: finished2},
		},
	}
	return workflow
}

// singleStepWorkflow returns a terminal workflow with exactly one step pod.
func singleStepWorkflow() *wfv1.Workflow {
	started := metav1.NewTime(time.Unix(1000, 0).UTC())
	finished := metav1.NewTime(time.Unix(1005, 0).UTC())
	workflow := &wfv1.Workflow{}
	workflow.Name = "wf"
	workflow.Namespace = testNamespace
	workflow.Annotations = map[string]string{
		runIDAnnotation:          "run-single",
		identityAnnotation:       "digest",
		PodNameVersionAnnotation: podNameVersionV2,
	}
	workflow.Spec = wfv1.WorkflowSpec{
		Entrypoint: "ci",
		Templates: []wfv1.Template{
			{Name: "ci", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "build", Template: "build"},
			}}},
			{Name: "build", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "compile", Template: "compile"},
			}}},
			{Name: "compile", Container: &corev1.Container{Image: "golang:1.26"}},
		},
	}
	workflow.Status = wfv1.WorkflowStatus{
		Phase: wfv1.WorkflowSucceeded,
		Nodes: wfv1.Nodes{
			"root": {ID: "root", Name: "wf", DisplayName: "wf",
				Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeSucceeded},
			"build-burn": {ID: "build-burn", Name: "wf.build", DisplayName: "build",
				Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeSucceeded, BoundaryID: "root"},
			"compile-pod": {ID: "compile-pod", Name: "wf.build.compile", DisplayName: "compile",
				TemplateName: "compile", Type: wfv1.NodeTypePod, Phase: wfv1.NodeSucceeded,
				BoundaryID: "build-burn", StartedAt: started, FinishedAt: finished},
		},
	}
	return workflow
}

type staticWorkflowClient struct {
	workflow *wfv1.Workflow
}

func (client *staticWorkflowClient) Create(_ context.Context, _ *wfv1.Workflow, _ metav1.CreateOptions) (*wfv1.Workflow, error) {
	return nil, errors.New("staticWorkflowClient: Create not expected")
}

func (client *staticWorkflowClient) Get(_ context.Context, _ string, _ metav1.GetOptions) (*wfv1.Workflow, error) {
	return client.workflow.DeepCopy(), nil
}

func (client *staticWorkflowClient) Delete(_ context.Context, _ string, _ metav1.DeleteOptions) error {
	return errors.New("staticWorkflowClient: Delete not expected")
}

// TestRunLogBudgetEnforcedAcrossSteps proves that a newline-dense multi-step
// workflow whose aggregate output exceeds the per-run byte ceiling causes Wait
// to return an error naming the offending step, making the run red.
func TestRunLogBudgetEnforcedAcrossSteps(t *testing.T) {
	t.Parallel()
	workflow := budgetTestWorkflow()
	client := &staticWorkflowClient{workflow: workflow}

	// A small aggregate budget: large enough for the first step's progress
	// markers and some log output, small enough that the second step's log
	// breaches it.
	config := testConfig()
	config.MaxRunLogBytes = 2048

	controller, err := NewController(client, nil, config)
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	// Each step produces 60 lines of output. With prefixing, each line is
	// roughly 30 bytes, yielding ~1800 bytes per step — enough to breach a
	// 2048-byte aggregate budget on the second step.
	controller.WithPodLogReader(func(_ context.Context, _, _, _ string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(strings.Repeat("output line here\n", 60))), nil
	})

	var destination bytes.Buffer
	_, waitErr := controller.Wait(context.Background(), "wf", "run-budget", &destination)
	if waitErr == nil {
		t.Fatal("expected Wait to return an error when the aggregate budget is exceeded")
	}
	// The error must name the step that caused the breach.
	errText := waitErr.Error()
	if !strings.Contains(errText, "budget") {
		t.Fatalf("error does not mention budget: %v", waitErr)
	}
	// The error must identify the step by its burn/step name. The exact step
	// depends on which one's log replay crosses the ceiling; either name
	// appearing in the error satisfies the requirement.
	if !strings.Contains(errText, "compile") && !strings.Contains(errText, "unit") {
		t.Fatalf("error does not name any step: %v", waitErr)
	}
	// Some output must have been written before the breach.
	if destination.Len() == 0 {
		t.Fatal("destination received no output before the budget was hit")
	}
}

// brokenWriter fails every write with a non-budget error, simulating a
// destination that becomes unreachable (disk full, broken pipe, etc.).
type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) {
	return 0, errors.New("destination unreachable")
}

// TestProgressWriterFailurePropagatesFromWait proves that progress-marker
// write failures (stored in reporter.failed) surface as an error from
// Controller.Wait, so a run with a degraded evidence trail is never reported
// as green.
func TestProgressWriterFailurePropagatesFromWait(t *testing.T) {
	t.Parallel()
	workflow := singleStepWorkflow()
	client := &staticWorkflowClient{workflow: workflow}

	config := testConfig()
	controller, err := NewController(client, nil, config)
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	controller.WithPodLogReader(func(_ context.Context, _, _, _ string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("log output\n")), nil
	})

	_, waitErr := controller.Wait(context.Background(), "wf", "run-single", brokenWriter{})
	if waitErr == nil {
		t.Fatal("expected Wait to return an error when progress writes fail")
	}
	if !strings.Contains(waitErr.Error(), "progress") {
		t.Fatalf("error should mention progress degradation: %v", waitErr)
	}
}

// TestPerStepBudgetUnchangedWithAggregateBudget verifies that the per-step
// 32 MiB budget is enforced independently of the aggregate run budget. A
// step whose output exceeds the per-step ceiling is truncated by the per-step
// check, not by the aggregate ceiling.
func TestPerStepBudgetUnchangedWithAggregateBudget(t *testing.T) {
	t.Parallel()
	step := StepResult{Burn: "test", Step: "log"}
	prefix := "[test/log] "
	lineContent := "x\n"
	// Enough lines to exceed the 32 MiB per-step budget.
	lineCount := (maxStepLogBytes / int64(len(prefix)+len(lineContent))) + 1000
	input := strings.Repeat(lineContent, int(lineCount))

	// The aggregate budget is ten times the per-step budget — it must not
	// be the binding constraint.
	var underlying bytes.Buffer
	budget := &runBudgetWriter{destination: &underlying, remaining: maxStepLogBytes * 10}
	err := copyPrefixed(budget, step, strings.NewReader(input))
	if err == nil {
		t.Fatal("expected per-step budget error")
	}
	if errors.Is(err, errRunLogBudget) {
		t.Fatal("aggregate budget error should not precede the per-step budget")
	}
	if !strings.Contains(err.Error(), "budget") {
		t.Fatalf("unexpected error: %v", err)
	}
	// The aggregate budget must still have headroom: the per-step check
	// fired before the aggregate was exhausted.
	if budget.remaining <= 0 {
		t.Fatal("aggregate budget should not be exhausted by a per-step truncation")
	}
}
