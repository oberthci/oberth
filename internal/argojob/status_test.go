package argojob

import (
	"fmt"
	"hash/fnv"
	"io"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/runprogress"
)

// upstreamGeneratePodName is a transcription of
// argo-workflows/workflow/util.GeneratePodName. Keeping an independent copy
// here means PodName is checked against the upstream algorithm rather than
// against itself; importing workflow/util directly would pull the whole
// controller into the server binary.
func upstreamGeneratePodName(workflowName, nodeName, templateName, nodeID string, v1 bool) string {
	if v1 {
		return nodeID
	}
	if workflowName == nodeName {
		return workflowName
	}
	prefix := workflowName
	if templateName != "" {
		prefix = fmt.Sprintf("%s-%s", prefix, templateName)
	}
	if len(prefix) > 253-10-1 {
		prefix = prefix[0 : 253-10-1]
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(nodeName))
	return fmt.Sprintf("%s-%v", prefix, h.Sum32())
}

// TestPodNameMatchesArgo pins the reimplementation. Getting this wrong does not
// fail loudly -- it silently produces a run with no step logs, which is how the
// first live run against a real controller surfaced it.
func TestPodNameMatchesArgo(t *testing.T) {
	cases := []struct {
		workflow, node, template, id string
	}{
		{"oberth-run-abc", "oberth-run-abc.setup.prepare", "say", "oberth-run-abc-3592249494"},
		{"oberth-run-abc", "oberth-run-abc", "", "oberth-run-abc"},
		{"oberth-run-abc", "oberth-run-abc.verify", "", "oberth-run-abc-99"},
		{"a-very-long-workflow-name-" + string(make([]byte, 300)), "n", "t", "id"},
	}
	for _, testCase := range cases {
		workflow := &wfv1.Workflow{}
		workflow.Name = testCase.workflow
		node := wfv1.NodeStatus{Name: testCase.node, TemplateName: testCase.template, ID: testCase.id}

		want := upstreamGeneratePodName(testCase.workflow, testCase.node, testCase.template, testCase.id, false)
		if got := PodName(workflow, node); got != want {
			t.Errorf("PodName(%q, %q, %q) = %q, want %q", testCase.workflow, testCase.node, testCase.template, got, want)
		}

		workflow.Annotations = map[string]string{PodNameVersionAnnotation: "v1"}
		wantV1 := upstreamGeneratePodName(testCase.workflow, testCase.node, testCase.template, testCase.id, true)
		if got := PodName(workflow, node); got != wantV1 {
			t.Errorf("v1 PodName = %q, want %q", got, wantV1)
		}
	}
}

func TestBuildPinsThePodNameVersion(t *testing.T) {
	built, err := Build(testConfig(), testRequest("ci", greedyDocument))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if built.Annotations[PodNameVersionAnnotation] != podNameVersionV2 {
		t.Fatalf("pod name version annotation = %q, want %q",
			built.Annotations[PodNameVersionAnnotation], podNameVersionV2)
	}
}

// nestedWorkflow is the node tree a two-burn pipeline produces: an entrypoint
// DAG whose tasks are burns, each burn a nested DAG of step Pods.
//
// The node types are the ones Argo's controller really writes. In particular a
// task the DAG omitted or a `when` guard removed is created with
// Type: NodeTypeSkipped, never Type: NodeTypePod with an Omitted phase --
// dag.go initializes those nodes through initializeNode(..., NodeTypeSkipped,
// ...). This fixture asserted the second combination for a long time, so the
// skipped mapping was pinned by a test while the real path discarded the node
// before its phase was ever examined.
func nestedWorkflow() *wfv1.Workflow {
	started := metav1.NewTime(time.Unix(1000, 0).UTC())
	finished := metav1.NewTime(time.Unix(1010, 0).UTC())
	workflow := &wfv1.Workflow{}
	workflow.Name = "wf"
	workflow.Spec = wfv1.WorkflowSpec{
		Entrypoint: "ci",
		Templates: []wfv1.Template{
			{Name: "ci", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "setup", Template: "setup"},
				{Name: "verify", Template: "verify"},
				{Name: "solo", Template: "solo"},
				{Name: "unreached", Template: "unreached"},
			}}},
			{Name: "setup", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "prepare", Template: "prepare"}}}},
			{Name: "verify", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "lint", Template: "lint"},
				{Name: "omitted", Template: "omitted"},
			}}},
			{Name: "prepare", Container: &corev1.Container{Image: "golang:1.26"}},
			{Name: "lint", Container: &corev1.Container{Image: "golang:1.26"}},
			{Name: "omitted", Container: &corev1.Container{Image: "golang:1.26"}},
			{Name: "solo", Container: &corev1.Container{Image: "golang:1.26"}},
			{Name: "unreached", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "never", Template: "prepare"}}}},
		},
	}
	workflow.Status = wfv1.WorkflowStatus{
		Phase: wfv1.WorkflowSucceeded,
		Nodes: wfv1.Nodes{
			"root":   {ID: "root", Name: "wf", DisplayName: "wf", Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeSucceeded},
			"setup":  {ID: "setup", Name: "wf.setup", DisplayName: "setup", Type: wfv1.NodeTypeDAG, BoundaryID: "root", Phase: wfv1.NodeSucceeded},
			"verify": {ID: "verify", Name: "wf.verify", DisplayName: "verify", Type: wfv1.NodeTypeDAG, BoundaryID: "root", Phase: wfv1.NodeSucceeded},
			"prepare": {ID: "prepare", Name: "wf.setup.prepare", DisplayName: "prepare", TemplateName: "prepare",
				Type: wfv1.NodeTypePod, BoundaryID: "setup", Phase: wfv1.NodeSucceeded, StartedAt: started, FinishedAt: finished},
			"lint": {ID: "lint", Name: "wf.verify.lint", DisplayName: "lint", TemplateName: "lint",
				Type: wfv1.NodeTypePod, BoundaryID: "verify", Phase: wfv1.NodeSucceeded, StartedAt: finished, FinishedAt: finished},
			"solo": {ID: "solo", Name: "wf.solo", DisplayName: "solo", TemplateName: "solo",
				Type: wfv1.NodeTypePod, BoundaryID: "root", Phase: wfv1.NodeSucceeded, StartedAt: finished, FinishedAt: finished},
			// A task whose `depends` was never satisfied: a Skipped node over a
			// Pod-producing template, which is a step that did not run.
			"omitted": {ID: "omitted", Name: "wf.verify.omitted", DisplayName: "omitted", TemplateName: "omitted",
				Type: wfv1.NodeTypeSkipped, BoundaryID: "verify", Phase: wfv1.NodeOmitted},
			// A whole burn the DAG omitted. Also a Skipped node, but over a DAG
			// template: it is a boundary, not a step, and must not become one.
			"unreached": {ID: "unreached", Name: "wf.unreached", DisplayName: "unreached", TemplateName: "unreached",
				Type: wfv1.NodeTypeSkipped, BoundaryID: "root", Phase: wfv1.NodeOmitted},
			"notapod":  {ID: "notapod", Name: "wf.verify(0)", DisplayName: "verify(0)", Type: wfv1.NodeTypeStepGroup, BoundaryID: "verify", Phase: wfv1.NodeSucceeded},
			"retrywrp": {ID: "retrywrp", Name: "wf.verify.lint(0)", DisplayName: "lint(0)", Type: wfv1.NodeTypeRetry, BoundaryID: "verify", Phase: wfv1.NodeSucceeded},
		},
	}
	return workflow
}

// TestStepResultsMapsNestedDAGsOntoBurnsAndSteps proves the two-level model
// falls out of an idiomatic Argo pipeline without any Oberth-specific naming.
func TestStepResultsMapsNestedDAGsOntoBurnsAndSteps(t *testing.T) {
	results := StepResults(nestedWorkflow())
	found := map[string]string{}
	for _, result := range results {
		found[result.Step] = result.Burn
	}
	for step, burn := range map[string]string{
		"prepare": "setup",
		"lint":    "verify",
		"omitted": "verify",
		// A Pod directly under the entrypoint is a burn of one step.
		"solo": "solo",
	} {
		if found[step] != burn {
			t.Errorf("step %q attributed to burn %q, want %q", step, found[step], burn)
		}
	}
	// Pod nodes and skipped Pod-producing tasks become steps; StepGroup, Retry
	// and the skipped invocation of a whole nested DAG are structure.
	if len(results) != 4 {
		t.Fatalf("got %d step results, want 4: %+v", len(results), results)
	}
	for _, result := range results {
		if result.Step == "unreached" {
			t.Fatalf("a skipped nested-DAG invocation became a step: %+v", result)
		}
		if result.Step == "omitted" {
			if result.Status != runprogress.StepSkipped {
				t.Errorf("omitted step status = %q, want %q", result.Status, runprogress.StepSkipped)
			}
			// There is no Pod behind a node that never ran, so nothing must
			// try to replay one's log.
			if result.PodName != "" {
				t.Errorf("skipped step carries pod name %q", result.PodName)
			}
		}
	}
	for index, result := range results {
		if result.Ordinal != index {
			t.Errorf("ordinal %d out of order in %+v", result.Ordinal, result)
		}
	}
}

func TestStripRetrySuffix(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"publish(0)", "publish"},
		{"publish(1)", "publish"},
		{"publish(42)", "publish"},
		{"release-sign(0)", "release-sign"},
		{"publish", "publish"},           // no suffix
		{"publish()", "publish()"},       // empty parens: not a retry index
		{"publish(abc)", "publish(abc)"}, // non-digit: not a retry index
		{"(0)", "(0)"},                   // nothing before the paren
		{"", ""},                         // empty string
		{"a(0)(1)", "a(0)"},              // strips only the outermost suffix
	} {
		if got := stripRetrySuffix(tc.input); got != tc.want {
			t.Errorf("stripRetrySuffix(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestStepResultsStripsRetryAttemptSuffix proves that a retryStrategy-wrapped
// pod's "(0)" suffix is stripped so the step result matches the planned step
// name. Without this, every retryStrategy template produces a phantom pending
// row ("publish") alongside the real result ("publish(0)").
func TestStepResultsStripsRetryAttemptSuffix(t *testing.T) {
	started := metav1.NewTime(time.Unix(1000, 0).UTC())
	finished := metav1.NewTime(time.Unix(1010, 0).UTC())

	workflow := &wfv1.Workflow{}
	workflow.Name = "rel"
	workflow.Spec = wfv1.WorkflowSpec{
		Entrypoint: "pipeline",
		Templates: []wfv1.Template{
			{Name: "pipeline", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "release", Template: "release-burn"},
			}}},
			{Name: "release-burn", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "build", Template: "build-tmpl"},
				{Name: "publish", Template: "publish-tmpl"},
			}}},
			{Name: "build-tmpl", Container: &corev1.Container{Image: "golang:1.26"}},
			{Name: "publish-tmpl", Container: &corev1.Container{Image: "alpine"}},
		},
	}
	workflow.Status = wfv1.WorkflowStatus{
		Phase: wfv1.WorkflowSucceeded,
		Nodes: wfv1.Nodes{
			"root": {ID: "root", Name: "rel", DisplayName: "rel",
				Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeSucceeded},
			"release": {ID: "release", Name: "rel.release", DisplayName: "release",
				Type: wfv1.NodeTypeDAG, BoundaryID: "root", Phase: wfv1.NodeSucceeded},
			// build: regular pod, no retry wrapper.
			"build": {ID: "build", Name: "rel.release.build", DisplayName: "build",
				TemplateName: "build-tmpl", Type: wfv1.NodeTypePod, BoundaryID: "release",
				Phase: wfv1.NodeSucceeded, StartedAt: started, FinishedAt: finished},
			// publish: wrapped in a Retry node because the template has retryStrategy.
			"pub-retry": {ID: "pub-retry", Name: "rel.release.publish",
				DisplayName: "publish", Type: wfv1.NodeTypeRetry,
				BoundaryID: "release", Phase: wfv1.NodeSucceeded,
				Children: []string{"pub-0"}},
			// The pod child carries the "(0)" attempt suffix.
			"pub-0": {ID: "pub-0", Name: "rel.release.publish(0)",
				DisplayName: "publish(0)", TemplateName: "publish-tmpl",
				Type: wfv1.NodeTypePod, BoundaryID: "release",
				Phase: wfv1.NodeSucceeded, StartedAt: started, FinishedAt: finished},
		},
	}

	results := StepResults(workflow)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2: %+v", len(results), results)
	}

	found := map[string]StepResult{}
	for _, r := range results {
		found[r.Step] = r
	}

	// The retry-wrapped pod's suffix must be stripped: "publish(0)" -> "publish".
	if _, ok := found["publish"]; !ok {
		t.Error("expected step 'publish' (retry suffix stripped); not found")
		for _, r := range results {
			t.Logf("  got burn=%q step=%q", r.Burn, r.Step)
		}
	}
	if _, ok := found["publish(0)"]; ok {
		t.Error("step 'publish(0)' should have been stripped to 'publish'")
	}
	// Both steps belong to the "release" burn.
	for _, step := range []string{"build", "publish"} {
		if r, ok := found[step]; ok && r.Burn != "release" {
			t.Errorf("step %q burn = %q, want 'release'", step, r.Burn)
		}
	}
}

// TestStepResultsDoesNotStripNonRetryNodes proves the suffix stripping is
// scoped to retry children: a regular pod whose name happens to end with
// digits in parentheses (unlikely but legal) is not touched.
func TestStepResultsDoesNotStripNonRetryNodes(t *testing.T) {
	started := metav1.NewTime(time.Unix(1000, 0).UTC())
	finished := metav1.NewTime(time.Unix(1010, 0).UTC())

	workflow := &wfv1.Workflow{}
	workflow.Name = "wf"
	workflow.Spec = wfv1.WorkflowSpec{
		Entrypoint: "ci",
		Templates: []wfv1.Template{
			{Name: "ci", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "step", Template: "run"},
			}}},
			{Name: "run", Container: &corev1.Container{Image: "alpine"}},
		},
	}
	workflow.Status = wfv1.WorkflowStatus{
		Phase: wfv1.WorkflowSucceeded,
		Nodes: wfv1.Nodes{
			"root": {ID: "root", Name: "wf", DisplayName: "wf",
				Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeSucceeded},
			// No Retry wrapper: this pod is NOT a retry child.
			"step": {ID: "step", Name: "wf.step", DisplayName: "step",
				TemplateName: "run", Type: wfv1.NodeTypePod, BoundaryID: "root",
				Phase: wfv1.NodeSucceeded, StartedAt: started, FinishedAt: finished},
		},
	}

	results := StepResults(workflow)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Step != "step" {
		t.Errorf("step = %q, want 'step'", results[0].Step)
	}
}

// TestRetrySuccessDoesNotSetFailedStep proves that a step whose first attempt
// failed but whose retry succeeded does not mark the run as failed. This is
// the exact scenario from issue #27: completionFor() iterated all attempt rows
// and set FailedStep from the failed first attempt, then result() flipped a
// phase-Succeeded Workflow to failed with "successful Workflow reported a
// failed step".
func TestRetrySuccessDoesNotSetFailedStep(t *testing.T) {
	attempt0Start := metav1.NewTime(time.Unix(1000, 0).UTC())
	attempt0End := metav1.NewTime(time.Unix(1010, 0).UTC())
	attempt1Start := metav1.NewTime(time.Unix(1015, 0).UTC())
	attempt1End := metav1.NewTime(time.Unix(1025, 0).UTC())

	workflow := &wfv1.Workflow{}
	workflow.Name = "rel"
	workflow.Spec = wfv1.WorkflowSpec{
		Entrypoint: "pipeline",
		Templates: []wfv1.Template{
			{Name: "pipeline", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "release", Template: "release-burn"},
			}}},
			{Name: "release-burn", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "publish", Template: "publish-tmpl"},
			}}},
			{Name: "publish-tmpl", Container: &corev1.Container{Image: "alpine"}},
		},
	}
	workflow.Status = wfv1.WorkflowStatus{
		Phase: wfv1.WorkflowSucceeded,
		Nodes: wfv1.Nodes{
			"root": {ID: "root", Name: "rel", DisplayName: "rel",
				Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeSucceeded},
			"release": {ID: "release", Name: "rel.release", DisplayName: "release",
				Type: wfv1.NodeTypeDAG, BoundaryID: "root", Phase: wfv1.NodeSucceeded},
			"pub-retry": {ID: "pub-retry", Name: "rel.release.publish",
				DisplayName: "publish", Type: wfv1.NodeTypeRetry,
				BoundaryID: "release", Phase: wfv1.NodeSucceeded,
				Children: []string{"pub-0", "pub-1"}},
			"pub-0": {ID: "pub-0", Name: "rel.release.publish(0)",
				DisplayName: "publish(0)", TemplateName: "publish-tmpl",
				Type: wfv1.NodeTypePod, BoundaryID: "release",
				Phase: wfv1.NodeFailed, StartedAt: attempt0Start, FinishedAt: attempt0End},
			"pub-1": {ID: "pub-1", Name: "rel.release.publish(1)",
				DisplayName: "publish(1)", TemplateName: "publish-tmpl",
				Type: wfv1.NodeTypePod, BoundaryID: "release",
				Phase: wfv1.NodeSucceeded, StartedAt: attempt1Start, FinishedAt: attempt1End},
		},
	}

	results := StepResults(workflow)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (deduplicated to final attempt): %+v", len(results), results)
	}
	if results[0].Step != "publish" {
		t.Errorf("step = %q, want 'publish'", results[0].Step)
	}
	if results[0].Status != runprogress.StepPassed {
		t.Errorf("status = %q, want %q (the final attempt passed)", results[0].Status, runprogress.StepPassed)
	}

	completion := completionFor(workflow)
	if completion.FailedStep != "" {
		t.Errorf("FailedStep = %q, want empty (the retry succeeded)", completion.FailedStep)
	}
	if !completion.Succeeded {
		t.Error("a succeeded Workflow with a retried step reported failure")
	}
}

// TestRetryAllFailedSetsFailedStep proves that when every retry attempt fails,
// the step is correctly reported as failed.
func TestRetryAllFailedSetsFailedStep(t *testing.T) {
	attempt0Start := metav1.NewTime(time.Unix(1000, 0).UTC())
	attempt0End := metav1.NewTime(time.Unix(1010, 0).UTC())
	attempt1Start := metav1.NewTime(time.Unix(1015, 0).UTC())
	attempt1End := metav1.NewTime(time.Unix(1025, 0).UTC())

	workflow := &wfv1.Workflow{}
	workflow.Name = "rel"
	workflow.Spec = wfv1.WorkflowSpec{
		Entrypoint: "pipeline",
		Templates: []wfv1.Template{
			{Name: "pipeline", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "release", Template: "release-burn"},
			}}},
			{Name: "release-burn", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "publish", Template: "publish-tmpl"},
			}}},
			{Name: "publish-tmpl", Container: &corev1.Container{Image: "alpine"}},
		},
	}
	workflow.Status = wfv1.WorkflowStatus{
		Phase: wfv1.WorkflowFailed,
		Nodes: wfv1.Nodes{
			"root": {ID: "root", Name: "rel", DisplayName: "rel",
				Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeFailed},
			"release": {ID: "release", Name: "rel.release", DisplayName: "release",
				Type: wfv1.NodeTypeDAG, BoundaryID: "root", Phase: wfv1.NodeFailed},
			"pub-retry": {ID: "pub-retry", Name: "rel.release.publish",
				DisplayName: "publish", Type: wfv1.NodeTypeRetry,
				BoundaryID: "release", Phase: wfv1.NodeFailed,
				Children: []string{"pub-0", "pub-1"}},
			"pub-0": {ID: "pub-0", Name: "rel.release.publish(0)",
				DisplayName: "publish(0)", TemplateName: "publish-tmpl",
				Type: wfv1.NodeTypePod, BoundaryID: "release",
				Phase: wfv1.NodeFailed, StartedAt: attempt0Start, FinishedAt: attempt0End},
			"pub-1": {ID: "pub-1", Name: "rel.release.publish(1)",
				DisplayName: "publish(1)", TemplateName: "publish-tmpl",
				Type: wfv1.NodeTypePod, BoundaryID: "release",
				Phase: wfv1.NodeFailed, StartedAt: attempt1Start, FinishedAt: attempt1End},
		},
	}

	completion := completionFor(workflow)
	if completion.FailedStep != "publish" {
		t.Errorf("FailedStep = %q, want 'publish'", completion.FailedStep)
	}
	if completion.FailedBurn != "release" {
		t.Errorf("FailedBurn = %q, want 'release'", completion.FailedBurn)
	}
	if completion.Succeeded {
		t.Error("a failed Workflow reported success")
	}
}

func TestStepResultsMapsArgoPhasesOntoOberthStatuses(t *testing.T) {
	for phase, want := range map[wfv1.NodePhase]runprogress.Status{
		wfv1.NodePending:   runprogress.StepQueued,
		wfv1.NodeRunning:   runprogress.StepRunning,
		wfv1.NodeSucceeded: runprogress.StepPassed,
		wfv1.NodeFailed:    runprogress.StepFailed,
		wfv1.NodeError:     runprogress.StepFailed,
		// Both mean "did not run, and that is normal".
		wfv1.NodeSkipped: runprogress.StepSkipped,
		wfv1.NodeOmitted: runprogress.StepSkipped,
	} {
		if got := stepStatus(phase); got != want {
			t.Errorf("stepStatus(%q) = %q, want %q", phase, got, want)
		}
	}
}

// TestExitCodeNeverReportsSuccessForAFailedNode is the guard against a failed
// step being recorded as a pass because Argo captured no exit code (an evicted
// Pod, an infrastructure error).
func TestExitCodeNeverReportsSuccessForAFailedNode(t *testing.T) {
	code := "7"
	if got := exitCode(wfv1.NodeStatus{Phase: wfv1.NodeFailed, Outputs: &wfv1.Outputs{ExitCode: &code}}); got != 7 {
		t.Fatalf("exit code = %d, want 7", got)
	}
	if got := exitCode(wfv1.NodeStatus{Phase: wfv1.NodeError}); got == 0 {
		t.Fatal("an errored node with no captured exit code reported success")
	}
	if got := exitCode(wfv1.NodeStatus{Phase: wfv1.NodeFailed}); got == 0 {
		t.Fatal("a failed node with no captured exit code reported success")
	}
	if got := exitCode(wfv1.NodeStatus{Phase: wfv1.NodeSucceeded}); got != 0 {
		t.Fatalf("a succeeded node reported exit code %d", got)
	}
	// A malformed exit code must not be read as zero.
	malformed := "not-a-number"
	if got := exitCode(wfv1.NodeStatus{Phase: wfv1.NodeFailed, Outputs: &wfv1.Outputs{ExitCode: &malformed}}); got == 0 {
		t.Fatal("a failed node with a malformed exit code reported success")
	}
}

func TestCompletionNamesTheFailedBurnAndStep(t *testing.T) {
	workflow := nestedWorkflow()
	node := workflow.Status.Nodes["lint"]
	node.Phase = wfv1.NodeFailed
	workflow.Status.Nodes["lint"] = node
	workflow.Status.Phase = wfv1.WorkflowFailed
	workflow.Status.Message = "lint failed"

	completion := completionFor(workflow)
	if completion.Succeeded {
		t.Fatal("a failed Workflow reported success")
	}
	if completion.FailedBurn != "verify" || completion.FailedStep != "lint" {
		t.Fatalf("failed burn/step = %q/%q, want verify/lint", completion.FailedBurn, completion.FailedStep)
	}
	if completion.Reason != "lint failed" {
		t.Fatalf("reason = %q", completion.Reason)
	}
}

// TestCompletionSucceededWithNonBlockingFailurePasses is the issue #13
// regression test: Argo's enhanced-depends semantics mark the Workflow
// Succeeded when the only failed task's failure is handled by every depends
// expression that references it (the homebrew pattern). The completion must
// stay Succeeded with no FailedBurn/FailedStep — so the engine's
// successful-Workflow-with-failed-step guard no longer flips the run — while
// the step row itself keeps its failed status for the step list.
// TestCompletionSucceededWorkflowWithFailedStepIsRed proves that a Succeeded
// Workflow with a terminally failed configured step is red. DAG depends
// expressions control Argo's execution flow but do not control Oberth's
// success gate: a step whose failure was tolerated by its dependents is still
// a run-level failure. This is how Homebrew publication failure correctly
// fails the release (finding 6, issue #28).
func TestCompletionSucceededWorkflowWithFailedStepIsRed(t *testing.T) {
	started := metav1.NewTime(time.Unix(1000, 0).UTC())
	finished := metav1.NewTime(time.Unix(1010, 0).UTC())

	workflow := &wfv1.Workflow{}
	workflow.Name = "rel"
	workflow.Spec = wfv1.WorkflowSpec{
		Entrypoint: "release",
		Templates: []wfv1.Template{
			{Name: "release", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "build", Template: "build-tmpl"},
				{Name: "publish", Template: "publish-tmpl", Depends: "build"},
				{Name: "homebrew", Template: "homebrew-tmpl", Depends: "publish"},
				{Name: "verify", Template: "verify-tmpl",
					Depends: "publish && (homebrew.Succeeded || homebrew.Failed || homebrew.Errored)"},
			}}},
			{Name: "build-tmpl", Container: &corev1.Container{Image: "golang:1.26"}},
			{Name: "publish-tmpl", Container: &corev1.Container{Image: "golang:1.26"}},
			{Name: "homebrew-tmpl", Container: &corev1.Container{Image: "golang:1.26"}},
			{Name: "verify-tmpl", Container: &corev1.Container{Image: "golang:1.26"}},
		},
	}
	workflow.Status = wfv1.WorkflowStatus{
		Phase: wfv1.WorkflowSucceeded,
		Nodes: wfv1.Nodes{
			"root": {ID: "root", Name: "rel", DisplayName: "rel",
				Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeSucceeded},
			"build": {ID: "build", Name: "rel.build", DisplayName: "build",
				TemplateName: "build-tmpl", Type: wfv1.NodeTypePod, BoundaryID: "root",
				Phase: wfv1.NodeSucceeded, StartedAt: started, FinishedAt: finished},
			"publish": {ID: "publish", Name: "rel.publish", DisplayName: "publish",
				TemplateName: "publish-tmpl", Type: wfv1.NodeTypePod, BoundaryID: "root",
				Phase: wfv1.NodeSucceeded, StartedAt: started, FinishedAt: finished},
			"homebrew": {ID: "homebrew", Name: "rel.homebrew", DisplayName: "homebrew",
				TemplateName: "homebrew-tmpl", Type: wfv1.NodeTypePod, BoundaryID: "root",
				Phase: wfv1.NodeFailed, StartedAt: started, FinishedAt: finished},
			"verify": {ID: "verify", Name: "rel.verify", DisplayName: "verify",
				TemplateName: "verify-tmpl", Type: wfv1.NodeTypePod, BoundaryID: "root",
				Phase: wfv1.NodeSucceeded, StartedAt: started, FinishedAt: finished},
		},
	}

	completion := completionFor(workflow)
	// A Succeeded Workflow with a terminally failed step must be red.
	if completion.Succeeded {
		t.Fatal("a Succeeded Workflow with a failed configured step must NOT stay Succeeded")
	}
	if completion.FailedBurn != "homebrew" || completion.FailedStep != "homebrew" {
		t.Errorf("FailedBurn/FailedStep = %q/%q, want homebrew/homebrew",
			completion.FailedBurn, completion.FailedStep)
	}

	// The individual step's status is still Failed in the Steps slice.
	found := false
	for _, step := range completion.Steps {
		if step.Step == "homebrew" {
			found = true
			if step.Status != runprogress.StepFailed {
				t.Errorf("homebrew step status = %q, want %q", step.Status, runprogress.StepFailed)
			}
		}
	}
	if !found {
		t.Error("homebrew step not found in completion.Steps")
	}
}

// TestCompletionFailedWorkflowIsNeverOverridden proves that a Workflow Argo
// marks Failed stays failed. A Failed phase always fails the run; the
// step-level failure attribution identifies the specific step.
func TestCompletionFailedWorkflowIsNeverOverridden(t *testing.T) {
	started := metav1.NewTime(time.Unix(1000, 0).UTC())
	finished := metav1.NewTime(time.Unix(1010, 0).UTC())

	workflow := &wfv1.Workflow{}
	workflow.Name = "rel"
	workflow.Spec = wfv1.WorkflowSpec{
		Entrypoint: "release",
		Templates: []wfv1.Template{
			{Name: "release", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "build", Template: "build-tmpl"},
				{Name: "publish", Template: "publish-tmpl", Depends: "build"},
				{Name: "homebrew", Template: "homebrew-tmpl", Depends: "publish"},
				{Name: "verify", Template: "verify-tmpl",
					Depends: "publish && (homebrew.Succeeded || homebrew.Failed || homebrew.Errored)"},
			}}},
			{Name: "build-tmpl", Container: &corev1.Container{Image: "golang:1.26"}},
			{Name: "publish-tmpl", Container: &corev1.Container{Image: "golang:1.26"}},
			{Name: "homebrew-tmpl", Container: &corev1.Container{Image: "golang:1.26"}},
			{Name: "verify-tmpl", Container: &corev1.Container{Image: "golang:1.26"}},
		},
	}
	// The dangerous shape: homebrew (non-blocking) failed, the Workflow was
	// failed by Argo (stop, deadline, controller failure), and verify never
	// reached pod creation — it has NO node row. Every failed row is
	// non-blocking, yet the blocking work did not run to completion.
	workflow.Status = wfv1.WorkflowStatus{
		Phase:   wfv1.WorkflowFailed,
		Message: "Stopped with strategy \"Stop\"",
		Nodes: wfv1.Nodes{
			"root": {ID: "root", Name: "rel", DisplayName: "rel",
				Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeFailed},
			"build": {ID: "build", Name: "rel.build", DisplayName: "build",
				TemplateName: "build-tmpl", Type: wfv1.NodeTypePod, BoundaryID: "root",
				Phase: wfv1.NodeSucceeded, StartedAt: started, FinishedAt: finished},
			"publish": {ID: "publish", Name: "rel.publish", DisplayName: "publish",
				TemplateName: "publish-tmpl", Type: wfv1.NodeTypePod, BoundaryID: "root",
				Phase: wfv1.NodeSucceeded, StartedAt: started, FinishedAt: finished},
			"homebrew": {ID: "homebrew", Name: "rel.homebrew", DisplayName: "homebrew",
				TemplateName: "homebrew-tmpl", Type: wfv1.NodeTypePod, BoundaryID: "root",
				Phase: wfv1.NodeFailed, StartedAt: started, FinishedAt: finished},
		},
	}

	completion := completionFor(workflow)
	if completion.Succeeded {
		t.Fatal("a Failed Workflow must never be overridden to Succeeded by step-row inspection")
	}
	if completion.FailedBurn != "homebrew" || completion.FailedStep != "homebrew" {
		t.Errorf("FailedBurn/FailedStep = %q/%q, want homebrew/homebrew",
			completion.FailedBurn, completion.FailedStep)
	}
	if completion.Reason == "" {
		t.Error("Reason must carry the Workflow's own failure message")
	}
}

// TestCompletionPreservesBlockingFailure proves that when multiple steps fail,
// the last failed step is attributed (chronological order).
func TestCompletionPreservesBlockingFailure(t *testing.T) {
	started := metav1.NewTime(time.Unix(1000, 0).UTC())
	finished := metav1.NewTime(time.Unix(1010, 0).UTC())

	workflow := &wfv1.Workflow{}
	workflow.Name = "rel"
	workflow.Spec = wfv1.WorkflowSpec{
		Entrypoint: "release",
		Templates: []wfv1.Template{
			{Name: "release", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "build", Template: "build-tmpl"},
				{Name: "homebrew", Template: "homebrew-tmpl", Depends: "build"},
				{Name: "verify", Template: "verify-tmpl",
					Depends: "build && (homebrew.Succeeded || homebrew.Failed || homebrew.Errored)"},
			}}},
			{Name: "build-tmpl", Container: &corev1.Container{Image: "golang:1.26"}},
			{Name: "homebrew-tmpl", Container: &corev1.Container{Image: "golang:1.26"}},
			{Name: "verify-tmpl", Container: &corev1.Container{Image: "golang:1.26"}},
		},
	}
	workflow.Status = wfv1.WorkflowStatus{
		Phase: wfv1.WorkflowFailed,
		Nodes: wfv1.Nodes{
			"root": {ID: "root", Name: "rel", DisplayName: "rel",
				Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeFailed},
			"build": {ID: "build", Name: "rel.build", DisplayName: "build",
				TemplateName: "build-tmpl", Type: wfv1.NodeTypePod, BoundaryID: "root",
				Phase: wfv1.NodeFailed, StartedAt: started, FinishedAt: finished},
			"homebrew": {ID: "homebrew", Name: "rel.homebrew", DisplayName: "homebrew",
				TemplateName: "homebrew-tmpl", Type: wfv1.NodeTypePod, BoundaryID: "root",
				Phase: wfv1.NodeFailed, StartedAt: started, FinishedAt: finished},
		},
	}

	completion := completionFor(workflow)
	if completion.Succeeded {
		t.Fatal("a Failed Workflow with a blocking failure must remain failed")
	}
	// Both steps failed; the last in iteration order (sorted by start time,
	// then burn name) is attributed. "build" < "homebrew" lexically.
	if completion.FailedBurn != "homebrew" {
		t.Errorf("FailedBurn = %q, want 'homebrew' (last failed step in iteration order)", completion.FailedBurn)
	}
}

// TestCompletionNoOverrideWithoutNonBlockingFailure proves that a Failed
// Workflow with only blocking failures is not overridden.
func TestCompletionNoOverrideWithoutNonBlockingFailure(t *testing.T) {
	started := metav1.NewTime(time.Unix(1000, 0).UTC())
	finished := metav1.NewTime(time.Unix(1010, 0).UTC())

	workflow := &wfv1.Workflow{}
	workflow.Name = "rel"
	workflow.Spec = wfv1.WorkflowSpec{
		Entrypoint: "release",
		Templates: []wfv1.Template{
			{Name: "release", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "build", Template: "build-tmpl"},
				{Name: "publish", Template: "publish-tmpl", Depends: "build"},
			}}},
			{Name: "build-tmpl", Container: &corev1.Container{Image: "golang:1.26"}},
			{Name: "publish-tmpl", Container: &corev1.Container{Image: "golang:1.26"}},
		},
	}
	workflow.Status = wfv1.WorkflowStatus{
		Phase: wfv1.WorkflowFailed,
		Nodes: wfv1.Nodes{
			"root": {ID: "root", Name: "rel", DisplayName: "rel",
				Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeFailed},
			"build": {ID: "build", Name: "rel.build", DisplayName: "build",
				TemplateName: "build-tmpl", Type: wfv1.NodeTypePod, BoundaryID: "root",
				Phase: wfv1.NodeFailed, StartedAt: started, FinishedAt: finished},
		},
	}

	completion := completionFor(workflow)
	if completion.Succeeded {
		t.Fatal("a Failed Workflow with only blocking failures must not be overridden")
	}
	if completion.FailedBurn != "build" || completion.FailedStep != "build" {
		t.Errorf("FailedBurn/FailedStep = %q/%q, want build/build", completion.FailedBurn, completion.FailedStep)
	}
}

// TestCompletionRetriedStepFinalPassIsGreen proves that a retried step whose
// final attempt succeeded is treated as a pass. deduplicateRetries keeps only
// the final attempt, so a step that failed once but then passed on retry does
// not make the run red.
func TestCompletionRetriedStepFinalPassIsGreen(t *testing.T) {
	started := metav1.NewTime(time.Unix(1000, 0).UTC())
	midpoint := metav1.NewTime(time.Unix(1005, 0).UTC())
	finished := metav1.NewTime(time.Unix(1010, 0).UTC())

	workflow := &wfv1.Workflow{}
	workflow.Name = "rel"
	workflow.Spec = wfv1.WorkflowSpec{
		Entrypoint: "release",
		Templates: []wfv1.Template{
			{Name: "release", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "publish", Template: "publish-tmpl"},
			}}},
			{Name: "publish-tmpl", Container: &corev1.Container{Image: "golang:1.26"},
				RetryStrategy: &wfv1.RetryStrategy{}},
		},
	}
	workflow.Status = wfv1.WorkflowStatus{
		Phase: wfv1.WorkflowSucceeded,
		Nodes: wfv1.Nodes{
			"root": {ID: "root", Name: "rel", DisplayName: "rel",
				Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeSucceeded},
			"retrywrp": {ID: "retrywrp", Name: "rel.publish", DisplayName: "publish",
				Type: wfv1.NodeTypeRetry, BoundaryID: "root", Phase: wfv1.NodeSucceeded,
				Children: []string{"attempt0", "attempt1"}},
			"attempt0": {ID: "attempt0", Name: "rel.publish(0)", DisplayName: "publish(0)",
				TemplateName: "publish-tmpl", Type: wfv1.NodeTypePod, BoundaryID: "root",
				Phase: wfv1.NodeFailed, StartedAt: started, FinishedAt: midpoint},
			"attempt1": {ID: "attempt1", Name: "rel.publish(1)", DisplayName: "publish(1)",
				TemplateName: "publish-tmpl", Type: wfv1.NodeTypePod, BoundaryID: "root",
				Phase: wfv1.NodeSucceeded, StartedAt: midpoint, FinishedAt: finished},
		},
	}

	completion := completionFor(workflow)
	if !completion.Succeeded {
		t.Fatal("a retried step whose final attempt succeeded must keep the run green")
	}
	if completion.FailedBurn != "" || completion.FailedStep != "" {
		t.Errorf("FailedBurn/FailedStep = %q/%q, want empty", completion.FailedBurn, completion.FailedStep)
	}
}

// TestProgressReporterEmitsAMonotonicStream proves the run log never shows a
// step going backwards or terminalizing twice, whatever the poll timing.
func TestProgressReporterEmitsAMonotonicStream(t *testing.T) {
	destination := &recordingWriter{}
	reporter := newProgressReporter(destination)
	started := time.Unix(1000, 0).UTC()
	finished := time.Unix(1010, 0).UTC()
	step := StepResult{Burn: "verify", Step: "lint", StartedAt: &started}

	step.Status = runprogress.StepRunning
	if reporter.observe(destination, step) {
		t.Fatal("a running step asked for its log to be replayed")
	}
	// A repeated observation of the same state emits nothing new.
	if reporter.observe(destination, step) {
		t.Fatal("a repeated running observation asked for a log replay")
	}
	step.Status, step.FinishedAt = runprogress.StepPassed, &finished
	if !reporter.observe(destination, step) {
		t.Fatal("a step reaching a terminal state did not ask for its log")
	}
	// A terminal step observed again must not replay its log a second time.
	if reporter.observe(destination, step) {
		t.Fatal("a terminal step asked for its log twice")
	}
	if reporter.failed != nil {
		t.Fatalf("reporter recorded a failure: %v", reporter.failed)
	}
}

// TestProgressReporterSynthesizesRunningForAFastStep proves a step that
// finishes between two polls still reads as a sequence in the run log.
func TestProgressReporterSynthesizesRunningForAFastStep(t *testing.T) {
	destination := &recordingWriter{}
	reporter := newProgressReporter(destination)
	started := time.Unix(1000, 0).UTC()
	finished := time.Unix(1010, 0).UTC()

	if !reporter.observe(destination, StepResult{
		Burn: "setup", Step: "prepare", Status: runprogress.StepPassed,
		StartedAt: &started, FinishedAt: &finished,
	}) {
		t.Fatal("a first-seen terminal step did not ask for its log")
	}
	body := destination.String()
	running, passed := 0, 0
	for _, line := range splitLines(body) {
		event, found, err := runprogress.ParseMarker([]byte(line + "\n"))
		if !found || err != nil {
			continue
		}
		switch event.Status {
		case runprogress.StepRunning:
			running++
		case runprogress.StepPassed:
			passed++
		}
	}
	if running != 1 || passed != 1 {
		t.Fatalf("emitted %d running and %d passed events, want 1 and 1:\n%s", running, passed, body)
	}
}

func splitLines(body string) []string {
	var lines []string
	current := ""
	for _, character := range body {
		if character == '\n' {
			lines = append(lines, current)
			current = ""
			continue
		}
		current += string(character)
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}

// TestCopyPrefixedAttributesEveryLine proves the run log index can attribute
// each line of a step's Pod log to its burn and step.
func TestCopyPrefixedAttributesEveryLine(t *testing.T) {
	destination := &recordingWriter{}
	if err := copyPrefixed(destination, StepResult{Burn: "setup", Step: "prepare"},
		stringReader("first\r\nsecond\nthird without newline")); err != nil {
		t.Fatalf("copyPrefixed: %v", err)
	}
	want := "[setup/prepare] first\n[setup/prepare] second\n[setup/prepare] third without newline\n"
	if destination.String() != want {
		t.Fatalf("got:\n%q\nwant:\n%q", destination.String(), want)
	}
}

func stringReader(body string) *stringBodyReader { return &stringBodyReader{body: []byte(body)} }

type stringBodyReader struct{ body []byte }

func (reader *stringBodyReader) Read(chunk []byte) (int, error) {
	if len(reader.body) == 0 {
		return 0, io.EOF
	}
	copied := copy(chunk, reader.body)
	reader.body = reader.body[copied:]
	return copied, nil
}

// TestProgressReporterRetryAttemptResetsFrozenStatus proves that a retry
// attempt after a terminal failure is not blocked by the monotonic rank check.
// Without this fix, the progressReporter freezes at the first attempt's failed
// status and silently drops the subsequent retry's Running and Passed
// transitions. The successful retry's log is never replayed.
func TestProgressReporterRetryAttemptResetsFrozenStatus(t *testing.T) {
	destination := &recordingWriter{}
	reporter := newProgressReporter(destination)
	started := time.Unix(1000, 0).UTC()
	failed := time.Unix(1005, 0).UTC()
	retryStart := time.Unix(1010, 0).UTC()
	retryEnd := time.Unix(1015, 0).UTC()

	step := StepResult{Burn: "release", Step: "publish", Ordinal: 0}

	// First attempt: running -> failed.
	step.Status = runprogress.StepRunning
	step.StartedAt = &started
	if reporter.observe(destination, step) {
		t.Fatal("running should not request log replay")
	}
	step.Status = runprogress.StepFailed
	step.FinishedAt = &failed
	if !reporter.observe(destination, step) {
		t.Fatal("first terminal (failed) should request log replay")
	}

	// Retry attempt: running again after a terminal failure.
	step.Status = runprogress.StepRunning
	step.StartedAt = &retryStart
	step.FinishedAt = nil
	if reporter.observe(destination, step) {
		t.Fatal("retry running should not request log replay")
	}

	// Retry succeeds.
	step.Status = runprogress.StepPassed
	step.FinishedAt = &retryEnd
	if !reporter.observe(destination, step) {
		t.Fatal("retry terminal (passed) should request log replay")
	}

	body := destination.String()
	var statuses []runprogress.Status
	for _, line := range splitLines(body) {
		event, found, err := runprogress.ParseMarker([]byte(line + "\n"))
		if !found || err != nil {
			continue
		}
		statuses = append(statuses, event.Status)
	}
	// Expect: running, failed, running (retry), passed.
	if len(statuses) != 4 {
		t.Fatalf("expected 4 status transitions, got %d: %v\n%s", len(statuses), statuses, body)
	}
	if statuses[0] != runprogress.StepRunning ||
		statuses[1] != runprogress.StepFailed ||
		statuses[2] != runprogress.StepRunning ||
		statuses[3] != runprogress.StepPassed {
		t.Fatalf("status sequence = %v, want [running, failed, running, passed]", statuses)
	}
}

// TestSkippedInlineStepBecomesStepResult proves that an inline task skipped by
// when/depends is reported as a skipped step rather than remaining pending
// forever. An inline template has an empty TemplateName and no children;
// without this fix it was silently discarded by stepNode.
func TestSkippedInlineStepBecomesStepResult(t *testing.T) {
	started := metav1.NewTime(time.Unix(1000, 0).UTC())
	finished := metav1.NewTime(time.Unix(1010, 0).UTC())

	workflow := &wfv1.Workflow{}
	workflow.Name = "wf"
	workflow.Spec = wfv1.WorkflowSpec{
		Entrypoint: "ci",
		Templates: []wfv1.Template{
			{Name: "ci", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "build", Template: "build-tmpl"},
				{Name: "optional-scan", Inline: &wfv1.Template{
					Container: &corev1.Container{Image: "scanner:latest"},
				}},
			}}},
			{Name: "build-tmpl", Container: &corev1.Container{Image: "golang:1.26"}},
		},
	}
	workflow.Status = wfv1.WorkflowStatus{
		Phase: wfv1.WorkflowSucceeded,
		Nodes: wfv1.Nodes{
			"root": {ID: "root", Name: "wf", DisplayName: "wf",
				Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeSucceeded},
			"build": {ID: "build", Name: "wf.build", DisplayName: "build",
				TemplateName: "build-tmpl", Type: wfv1.NodeTypePod, BoundaryID: "root",
				Phase: wfv1.NodeSucceeded, StartedAt: started, FinishedAt: finished},
			// Inline task skipped by a `when` guard: empty TemplateName, no children.
			"scan": {ID: "scan", Name: "wf.optional-scan", DisplayName: "optional-scan",
				TemplateName: "", Type: wfv1.NodeTypeSkipped, BoundaryID: "root",
				Phase: wfv1.NodeSkipped},
		},
	}

	results := StepResults(workflow)
	found := false
	for _, result := range results {
		if result.Step == "optional-scan" {
			found = true
			if result.Status != runprogress.StepSkipped {
				t.Errorf("skipped inline step status = %q, want %q", result.Status, runprogress.StepSkipped)
			}
			if result.PodName != "" {
				t.Errorf("skipped inline step has pod name %q", result.PodName)
			}
		}
	}
	if !found {
		t.Fatal("skipped inline step 'optional-scan' was not included in StepResults")
	}
}

// TestSkippedDAGWrapperNotMistakenForStep proves that a skipped DAG wrapper
// node (which has children) is not mistakenly treated as a step by the inline
// template acceptance path. Without this guard, skipped burn boundaries would
// appear as phantom step results.
func TestSkippedDAGWrapperNotMistakenForStep(t *testing.T) {
	workflow := nestedWorkflow()
	// The "unreached" node is a skipped DAG-template invocation with
	// TemplateName="unreached" and Children. It should NOT become a step.
	results := StepResults(workflow)
	for _, result := range results {
		if result.Step == "unreached" {
			t.Fatalf("a skipped DAG wrapper became a step: %+v", result)
		}
	}
}

// TestSkippedInlineDAGWithChildrenNotMistakenForStep proves that a skipped
// inline DAG wrapper (empty TemplateName, but has children) is not treated
// as a step.
func TestSkippedInlineDAGWithChildrenNotMistakenForStep(t *testing.T) {
	started := metav1.NewTime(time.Unix(1000, 0).UTC())
	finished := metav1.NewTime(time.Unix(1010, 0).UTC())

	workflow := &wfv1.Workflow{}
	workflow.Name = "wf"
	workflow.Spec = wfv1.WorkflowSpec{
		Entrypoint: "ci",
		Templates: []wfv1.Template{
			{Name: "ci", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "build", Template: "build-tmpl"},
			}}},
			{Name: "build-tmpl", Container: &corev1.Container{Image: "golang:1.26"}},
		},
	}
	workflow.Status = wfv1.WorkflowStatus{
		Phase: wfv1.WorkflowSucceeded,
		Nodes: wfv1.Nodes{
			"root": {ID: "root", Name: "wf", DisplayName: "wf",
				Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeSucceeded},
			"build": {ID: "build", Name: "wf.build", DisplayName: "build",
				TemplateName: "build-tmpl", Type: wfv1.NodeTypePod, BoundaryID: "root",
				Phase: wfv1.NodeSucceeded, StartedAt: started, FinishedAt: finished},
			// Skipped inline DAG wrapper — has children, empty template name.
			"inline-dag": {ID: "inline-dag", Name: "wf.inline-dag", DisplayName: "inline-dag",
				TemplateName: "", Type: wfv1.NodeTypeSkipped, BoundaryID: "root",
				Phase: wfv1.NodeOmitted, Children: []string{"child1"}},
		},
	}

	results := StepResults(workflow)
	for _, result := range results {
		if result.Step == "inline-dag" {
			t.Fatalf("a skipped inline DAG wrapper with children became a step: %+v", result)
		}
	}
}
