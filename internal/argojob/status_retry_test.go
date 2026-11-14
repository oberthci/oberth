package argojob

import (
	"bytes"
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// retryClock hands out strictly increasing node timestamps.
type retryClock struct{ at time.Time }

func (clock *retryClock) tick() metav1.Time {
	clock.at = clock.at.Add(5 * time.Second)
	return metav1.NewTime(clock.at)
}

// dagRetryWorkflow reproduces the node tree Argo writes for issue #1176: a
// DAG task ("address") whose template is itself a DAG carrying a
// retryStrategy. Argo wraps the invocation in a Retry node and runs each
// attempt as its own DAG node, "address(0)", "address(1)", ...; the pods of
// an attempt are bounded by that attempt node, not by the Retry node.
//
// attempts lists, per attempt, the phase of its "get" and "check" pods. A
// phase of "" means Argo never created that pod in this attempt.
func dagRetryWorkflow(phase wfv1.WorkflowPhase, attempts ...[2]wfv1.NodePhase) *wfv1.Workflow {
	clock := &retryClock{at: time.Unix(1_700_000_000, 0).UTC()}
	workflow := &wfv1.Workflow{}
	workflow.Name = "wf"
	workflow.Namespace = testNamespace
	workflow.Annotations = map[string]string{
		runIDAnnotation:    "run-retry",
		identityAnnotation: "digest",
	}
	limit := intstr.FromInt32(int32(len(attempts) - 1))
	workflow.Spec = wfv1.WorkflowSpec{
		Entrypoint: "ci",
		Templates: []wfv1.Template{
			{Name: "ci", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "create", Template: "create"},
				{Name: "address", Template: "address", Dependencies: []string{"create"}},
				{Name: "install", Template: "install", Dependencies: []string{"address"}},
			}}},
			{Name: "address", RetryStrategy: &wfv1.RetryStrategy{Limit: &limit},
				DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
					{Name: "get", Template: "get"},
					{Name: "check", Template: "check", Dependencies: []string{"get"}},
				}}},
			{Name: "create", Container: &corev1.Container{Image: "alpine"}},
			{Name: "get", Container: &corev1.Container{Image: "alpine"}},
			{Name: "check", Container: &corev1.Container{Image: "alpine"}},
			{Name: "install", Container: &corev1.Container{Image: "alpine"}},
		},
	}
	nodes := wfv1.Nodes{}
	pod := func(id, name, display, template, boundary string, phase wfv1.NodePhase) {
		started := clock.tick()
		nodes[id] = wfv1.NodeStatus{ID: id, Name: name, DisplayName: display, TemplateName: template,
			Type: wfv1.NodeTypePod, BoundaryID: boundary, Phase: phase,
			StartedAt: started, FinishedAt: clock.tick()}
	}
	nodes["root"] = wfv1.NodeStatus{ID: "root", Name: "wf", DisplayName: "wf", Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeSucceeded}
	pod("create", "wf.create", "create", "create", "root", wfv1.NodeSucceeded)

	retry := wfv1.NodeStatus{ID: "address", Name: "wf.address", DisplayName: "address",
		TemplateName: "address", Type: wfv1.NodeTypeRetry, BoundaryID: "root"}
	lastPassed := false
	for index, attempt := range attempts {
		id := "address-" + string(rune('0'+index))
		suffix := "(" + string(rune('0'+index)) + ")"
		retry.Children = append(retry.Children, id)
		attemptPhase := wfv1.NodeSucceeded
		for _, step := range attempt {
			if step == wfv1.NodeFailed || step == wfv1.NodeError {
				attemptPhase = wfv1.NodeFailed
			}
		}
		nodes[id] = wfv1.NodeStatus{ID: id, Name: "wf.address" + suffix, DisplayName: "address" + suffix,
			TemplateName: "address", Type: wfv1.NodeTypeDAG, BoundaryID: "root", Phase: attemptPhase}
		for position, name := range []string{"get", "check"} {
			if attempt[position] == "" {
				continue
			}
			pod(id+"-"+name, "wf.address"+suffix+"."+name, name, name, id, attempt[position])
		}
		lastPassed = attemptPhase == wfv1.NodeSucceeded
	}
	retry.Phase = wfv1.NodeFailed
	if lastPassed {
		retry.Phase = wfv1.NodeSucceeded
	}
	nodes["address"] = retry
	if lastPassed {
		pod("install", "wf.install", "install", "install", "root", wfv1.NodeSucceeded)
	}
	workflow.Status = wfv1.WorkflowStatus{Phase: phase, Nodes: nodes}
	return workflow
}

func stepKeys(results []StepResult) []string {
	keys := make([]string, 0, len(results))
	for _, result := range results {
		keys = append(keys, result.Stage+"/"+result.Step)
	}
	sort.Strings(keys)
	return keys
}

func stepByKey(t *testing.T, results []StepResult, key string) StepResult {
	t.Helper()
	for _, result := range results {
		if result.Stage+"/"+result.Step == key {
			return result
		}
	}
	t.Fatalf("no step %s in %v", key, stepKeys(results))
	return StepResult{}
}

// assertPlanned proves every recorded step resolves a planned row, so no
// planned row is left pending beside a suffixed duplicate.
func assertPlanned(t *testing.T, workflow *wfv1.Workflow, results []StepResult) {
	t.Helper()
	planned, err := argoworkflow.PlannedSteps(workflow)
	if err != nil {
		t.Fatalf("PlannedSteps: %v", err)
	}
	keys := map[string]bool{}
	for _, step := range planned {
		keys[step.Stage+"/"+step.Step] = true
	}
	for _, result := range results {
		if !keys[result.Stage+"/"+result.Step] {
			t.Errorf("observed step %s/%s is not in the plan %v", result.Stage, result.Step, keys)
		}
	}
}

// TestDAGRetryThenPassIsPassed is issue #1176: the first attempt of a retried
// DAG failed its check, the second attempt passed, and Argo marked the
// Workflow Succeeded. The step is passed, the run is green, and the failed
// first attempt is still reported as an attempt.
func TestDAGRetryThenPassIsPassed(t *testing.T) {
	workflow := dagRetryWorkflow(wfv1.WorkflowSucceeded,
		[2]wfv1.NodePhase{wfv1.NodeSucceeded, wfv1.NodeFailed},
		[2]wfv1.NodePhase{wfv1.NodeSucceeded, wfv1.NodeSucceeded})

	results := fixtureStepResults(workflow)
	want := "address/check address/get create/create install/install"
	if got := strings.Join(stepKeys(results), " "); got != want {
		t.Fatalf("steps = %q, want %q", got, want)
	}
	assertPlanned(t, workflow, results)
	for _, result := range results {
		if result.Status != runprogress.StepPassed || result.Superseded {
			t.Errorf("%s/%s = %s superseded=%v, want passed", result.Stage, result.Step, result.Status, result.Superseded)
		}
	}
	check := stepByKey(t, results, "address/check")
	if check.NodeID != "address-1-check" || check.Attempt != "address(1)" {
		t.Errorf("address/check came from node %q attempt %q, want the second attempt", check.NodeID, check.Attempt)
	}

	completion := completionFor(workflow)
	if !completion.Succeeded || completion.FailedStage != "" || completion.FailedStep != "" {
		t.Fatalf("completion = succeeded %v failed %q/%q, want green", completion.Succeeded, completion.FailedStage, completion.FailedStep)
	}

	// The failed first attempt stays observable as a superseded attempt.
	var superseded []StepResult
	for _, attempt := range fixtureStepAttempts(workflow) {
		if attempt.Superseded {
			superseded = append(superseded, attempt)
		}
	}
	if len(superseded) != 2 {
		t.Fatalf("superseded attempts = %+v, want get and check of address(0)", superseded)
	}
	failed := stepByKey(t, superseded, "address/check")
	if failed.Status != runprogress.StepFailed || failed.Attempt != "address(0)" || failed.NodeID != "address-0-check" {
		t.Errorf("superseded check = %+v, want the failed address(0) attempt", failed)
	}
}

// TestDAGRetryEveryAttemptFailsIsFailed proves retries exhausted stay red and
// the failure is attributed to the last attempt, even when the last attempt
// failed at an earlier step than the first one did.
func TestDAGRetryEveryAttemptFailsIsFailed(t *testing.T) {
	for _, test := range []struct {
		name     string
		attempts [][2]wfv1.NodePhase
		failed   string
		node     string
	}{
		{"same step", [][2]wfv1.NodePhase{
			{wfv1.NodeSucceeded, wfv1.NodeFailed},
			{wfv1.NodeSucceeded, wfv1.NodeFailed},
		}, "check", "address-1-check"},
		{"earlier step", [][2]wfv1.NodePhase{
			{wfv1.NodeSucceeded, wfv1.NodeFailed},
			{wfv1.NodeSucceeded, wfv1.NodeFailed},
			{wfv1.NodeFailed, ""},
		}, "get", "address-2-get"},
	} {
		t.Run(test.name, func(t *testing.T) {
			workflow := dagRetryWorkflow(wfv1.WorkflowFailed, test.attempts...)
			results := fixtureStepResults(workflow)
			assertPlanned(t, workflow, results)
			final := stepByKey(t, results, "address/"+test.failed)
			if final.Status != runprogress.StepFailed || final.NodeID != test.node {
				t.Fatalf("address/%s = %s from %q, want failed from %q", test.failed, final.Status, final.NodeID, test.node)
			}
			completion := completionFor(workflow)
			if completion.Succeeded {
				t.Fatal("exhausted retries reported success")
			}
			if completion.FailedStage != "address" || completion.FailedStep != test.failed {
				t.Fatalf("FailedBurn/FailedStep = %q/%q, want address/%s", completion.FailedStage, completion.FailedStep, test.failed)
			}
		})
	}
}

// TestPodRetryEveryAttemptFailsPointsAtLastAttempt covers retryStrategy on a
// pod template: every attempt failed, so the step is failed and its record is
// the last attempt's.
func TestPodRetryEveryAttemptFailsPointsAtLastAttempt(t *testing.T) {
	clock := &retryClock{at: time.Unix(1_700_000_000, 0).UTC()}
	workflow := &wfv1.Workflow{}
	workflow.Name = "rel"
	workflow.Spec = wfv1.WorkflowSpec{Entrypoint: "pipeline", Templates: []wfv1.Template{
		{Name: "pipeline", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "release", Template: "release-burn"}}}},
		{Name: "release-burn", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "publish", Template: "publish-tmpl"}}}},
		{Name: "publish-tmpl", Container: &corev1.Container{Image: "alpine"}, RetryStrategy: &wfv1.RetryStrategy{}},
	}}
	nodes := wfv1.Nodes{
		"root":    {ID: "root", Name: "rel", DisplayName: "rel", Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeFailed},
		"release": {ID: "release", Name: "rel.release", DisplayName: "release", Type: wfv1.NodeTypeDAG, BoundaryID: "root", Phase: wfv1.NodeFailed},
		"retry": {ID: "retry", Name: "rel.release.publish", DisplayName: "publish", Type: wfv1.NodeTypeRetry,
			BoundaryID: "release", Phase: wfv1.NodeFailed, Children: []string{"pub-0", "pub-1", "pub-2"}},
	}
	for index, id := range []string{"pub-0", "pub-1", "pub-2"} {
		suffix := "(" + string(rune('0'+index)) + ")"
		code := string(rune('1' + index))
		nodes[id] = wfv1.NodeStatus{ID: id, Name: "rel.release.publish" + suffix, DisplayName: "publish" + suffix,
			TemplateName: "publish-tmpl", Type: wfv1.NodeTypePod, BoundaryID: "release", Phase: wfv1.NodeFailed,
			StartedAt: clock.tick(), FinishedAt: clock.tick(), Outputs: &wfv1.Outputs{ExitCode: &code}}
	}
	workflow.Status = wfv1.WorkflowStatus{Phase: wfv1.WorkflowFailed, Nodes: nodes}

	appendRetryHook(workflow, "retry")
	results := fixtureStepResults(workflow)
	if len(results) != 2 {
		t.Fatalf("results = %+v, want publish and cleanup rows", results)
	}
	hook := stepByKey(t, results, "release/cleanup")
	if hook.Attempt != "" || hook.Superseded {
		t.Fatalf("hook treated as retry: %+v", hook)
	}
	if got := stepByKey(t, results, "release/publish"); got.NodeID != "pub-2" || got.Status != runprogress.StepFailed || got.ExitCode != 3 || got.Attempt != "publish(2)" {
		t.Fatalf("publish = %+v, want the failed last attempt pub-2 exit 3", got)
	}
	completion := completionFor(workflow)
	if completion.Succeeded || completion.FailedStage != "release" || completion.FailedStep != "publish" {
		t.Fatalf("completion = %v %q/%q, want failed release/publish", completion.Succeeded, completion.FailedStage, completion.FailedStep)
	}
}

// TestNoRetryStrategyIsUnchanged pins that a pipeline with no Retry node
// keeps its rows, names and verdict: nothing is superseded, nothing is
// renamed, and a failed step still fails a Workflow Argo called Succeeded.
func TestNoRetryStrategyIsUnchanged(t *testing.T) {
	workflow := budgetTestWorkflow()
	results := fixtureStepResults(workflow)
	if got := strings.Join(stepKeys(results), " "); got != "build/compile test/unit" {
		t.Fatalf("steps = %q", got)
	}
	attempts := fixtureStepAttempts(workflow)
	if len(attempts) != len(results) {
		t.Fatalf("attempts = %d rows, results = %d rows", len(attempts), len(results))
	}
	for _, attempt := range attempts {
		if attempt.Superseded || attempt.Attempt != "" {
			t.Fatalf("a step outside any retryStrategy was treated as an attempt: %+v", attempt)
		}
	}
	if completion := completionFor(workflow); !completion.Succeeded {
		t.Fatalf("green workflow went red: %+v", completion)
	}

	failed := budgetTestWorkflow()
	failed.Status.Phase = wfv1.WorkflowFailed
	node := failed.Status.Nodes["unit-pod"]
	node.Phase = wfv1.NodeFailed
	failed.Status.Nodes["unit-pod"] = node
	completion := completionFor(failed)
	if completion.Succeeded || completion.FailedStage != "test" || completion.FailedStep != "unit" {
		t.Fatalf("failed step without retry = %v %q/%q, want failed test/unit", completion.Succeeded, completion.FailedStage, completion.FailedStep)
	}
}

// nestedRetryWorkflow is a retry nested inside a DAG burn: the "release" burn
// runs a "group" task whose DAG template has a retryStrategy, and inside it
// the "push" pod template has its own retryStrategy.
//
// Outer attempt group(0): push(0) failed, push(1) failed -> group(0) failed.
// Outer attempt group(1): push(0) passed.
//
// group(0)'s push(1) is the latest attempt of its own Retry node and still
// superseded, because the enclosing Retry node moved on to group(1).
func nestedRetryWorkflow(phase wfv1.WorkflowPhase, finalPush wfv1.NodePhase) *wfv1.Workflow {
	clock := &retryClock{at: time.Unix(1_700_000_000, 0).UTC()}
	workflow := &wfv1.Workflow{}
	workflow.Name = "rel"
	workflow.Spec = wfv1.WorkflowSpec{Entrypoint: "pipeline", Templates: []wfv1.Template{
		{Name: "pipeline", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "release", Template: "release-burn"}}}},
		{Name: "release-burn", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
			{Name: "prepare", Template: "prepare"},
			{Name: "group", Template: "group", Dependencies: []string{"prepare"}},
		}}},
		{Name: "group", RetryStrategy: &wfv1.RetryStrategy{}, DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
			{Name: "push", Template: "push"},
		}}},
		{Name: "prepare", Container: &corev1.Container{Image: "alpine"}},
		{Name: "push", Container: &corev1.Container{Image: "alpine"}, RetryStrategy: &wfv1.RetryStrategy{}},
	}}
	pod := func(id, name, display, template, boundary string, phase wfv1.NodePhase) wfv1.NodeStatus {
		return wfv1.NodeStatus{ID: id, Name: name, DisplayName: display, TemplateName: template,
			Type: wfv1.NodeTypePod, BoundaryID: boundary, Phase: phase, StartedAt: clock.tick(), FinishedAt: clock.tick()}
	}
	nodes := wfv1.Nodes{
		"root":    {ID: "root", Name: "rel", DisplayName: "rel", Type: wfv1.NodeTypeDAG},
		"release": {ID: "release", Name: "rel.release", DisplayName: "release", Type: wfv1.NodeTypeDAG, BoundaryID: "root"},
		"group": {ID: "group", Name: "rel.release.group", DisplayName: "group", TemplateName: "group",
			Type: wfv1.NodeTypeRetry, BoundaryID: "release", Children: []string{"group-0", "group-1"}},
		"group-0": {ID: "group-0", Name: "rel.release.group(0)", DisplayName: "group(0)", TemplateName: "group",
			Type: wfv1.NodeTypeDAG, BoundaryID: "release", Phase: wfv1.NodeFailed},
		"g0-push": {ID: "g0-push", Name: "rel.release.group(0).push", DisplayName: "push", TemplateName: "push",
			Type: wfv1.NodeTypeRetry, BoundaryID: "group-0", Phase: wfv1.NodeFailed, Children: []string{"g0-push-0", "g0-push-1"}},
		"group-1": {ID: "group-1", Name: "rel.release.group(1)", DisplayName: "group(1)", TemplateName: "group",
			Type: wfv1.NodeTypeDAG, BoundaryID: "release", Phase: finalPush},
		"g1-push": {ID: "g1-push", Name: "rel.release.group(1).push", DisplayName: "push", TemplateName: "push",
			Type: wfv1.NodeTypeRetry, BoundaryID: "group-1", Phase: finalPush, Children: []string{"g1-push-0"}},
	}
	nodes["prepare"] = pod("prepare", "rel.release.prepare", "prepare", "prepare", "release", wfv1.NodeSucceeded)
	nodes["g0-push-0"] = pod("g0-push-0", "rel.release.group(0).push(0)", "push(0)", "push", "group-0", wfv1.NodeFailed)
	nodes["g0-push-1"] = pod("g0-push-1", "rel.release.group(0).push(1)", "push(1)", "push", "group-0", wfv1.NodeFailed)
	nodes["g1-push-0"] = pod("g1-push-0", "rel.release.group(1).push(0)", "push(0)", "push", "group-1", finalPush)
	workflow.Status = wfv1.WorkflowStatus{Phase: phase, Nodes: nodes}
	return workflow
}

// TestNestedRetryInsideDAG covers a retried pod inside a retried DAG inside a
// DAG burn, both when the outer retry recovers and when it is exhausted.
func TestNestedRetryInsideDAG(t *testing.T) {
	t.Run("outer retry passes", func(t *testing.T) {
		workflow := nestedRetryWorkflow(wfv1.WorkflowSucceeded, wfv1.NodeSucceeded)
		results := fixtureStepResults(workflow)
		if got := strings.Join(stepKeys(results), " "); got != "group/push release/prepare" {
			t.Fatalf("steps = %q", got)
		}
		assertPlanned(t, workflow, results)
		push := stepByKey(t, results, "group/push")
		if push.Status != runprogress.StepPassed || push.NodeID != "g1-push-0" || push.Attempt != "push(0)" {
			t.Fatalf("group/push = %+v, want passed from g1-push-0", push)
		}
		superseded := map[string]bool{}
		for _, attempt := range fixtureStepAttempts(workflow) {
			if attempt.Superseded {
				superseded[attempt.NodeID] = true
			}
		}
		if len(superseded) != 2 || !superseded["g0-push-0"] || !superseded["g0-push-1"] {
			t.Fatalf("superseded = %v, want both pushes of group(0)", superseded)
		}
		if completion := completionFor(workflow); !completion.Succeeded || completion.FailedStep != "" {
			t.Fatalf("completion = %v %q/%q, want green", completion.Succeeded, completion.FailedStage, completion.FailedStep)
		}
	})
	t.Run("outer retry exhausted", func(t *testing.T) {
		workflow := nestedRetryWorkflow(wfv1.WorkflowFailed, wfv1.NodeFailed)
		push := stepByKey(t, fixtureStepResults(workflow), "group/push")
		if push.Status != runprogress.StepFailed || push.NodeID != "g1-push-0" {
			t.Fatalf("group/push = %+v, want failed from the last attempt g1-push-0", push)
		}
		completion := completionFor(workflow)
		if completion.Succeeded || completion.FailedStage != "group" || completion.FailedStep != "push" {
			t.Fatalf("completion = %v %q/%q, want failed group/push", completion.Succeeded, completion.FailedStage, completion.FailedStep)
		}
	})
}

// TestWaitResumedRetriedDAGIsGreen runs an adopted Workflow (#1172) whose
// retried DAG recovered through Wait end to end: the completion is green, the
// durable rows carry the planned names, and no attempt is reported twice.
func TestWaitResumedRetriedDAGIsGreen(t *testing.T) {
	workflow := dagRetryWorkflow(wfv1.WorkflowSucceeded,
		[2]wfv1.NodePhase{wfv1.NodeSucceeded, wfv1.NodeFailed},
		[2]wfv1.NodePhase{wfv1.NodeSucceeded, wfv1.NodeSucceeded})
	controller := &Controller{workflows: &staticWorkflowClient{workflow: workflow},
		config: Config{Namespace: workflow.Namespace, MaxRunLogBytes: 1 << 20}, idleInterval: time.Millisecond}
	var output bytes.Buffer
	completion, err := withWaitTestHub(t, controller).WaitResumed(context.Background(), workflow.Name, "run-retry", &output, Adoption{})
	if err != nil {
		t.Fatalf("WaitResumed: %v", err)
	}
	if !completion.Succeeded || completion.FailedStep != "" {
		t.Fatalf("completion = %v %q/%q, want green", completion.Succeeded, completion.FailedStage, completion.FailedStep)
	}
	if got := strings.Join(stepKeys(completion.Steps), " "); got != "address/check address/get create/create install/install" {
		t.Fatalf("durable steps = %q", got)
	}
	if strings.Contains(output.String(), "@@oberth-step-progress@@") {
		t.Fatal("step marker emitted")
	}
}

func appendRetryHook(workflow *wfv1.Workflow, retryID string) string {
	retry := workflow.Status.Nodes[retryID]
	id := retryID + "-exit"
	retry.Children = append(retry.Children, id)
	workflow.Status.Nodes[retryID] = retry
	workflow.Status.Nodes[id] = wfv1.NodeStatus{
		ID: id, Name: retry.Name + ".onExit", DisplayName: "cleanup",
		Type: wfv1.NodeTypePod, BoundaryID: retry.BoundaryID,
		Phase: wfv1.NodeSucceeded, NodeFlag: &wfv1.NodeFlag{Hooked: true},
	}
	return id
}

func TestDAGRetryWithExitHook(t *testing.T) {
	for _, final := range []wfv1.NodePhase{wfv1.NodeFailed, wfv1.NodeSucceeded} {
		t.Run(string(final), func(t *testing.T) {
			workflow := dagRetryWorkflow(wfv1.WorkflowSucceeded,
				[2]wfv1.NodePhase{wfv1.NodeSucceeded, wfv1.NodeFailed},
				[2]wfv1.NodePhase{wfv1.NodeSucceeded, final})
			// Argo can call the outer workflow successful despite exhausted retries.
			workflow.Spec.Templates[0].DAG.Tasks[1].ContinueOn = &wfv1.ContinueOn{Failed: true}
			workflow.Spec.Templates[0].DAG.Tasks[1].Hooks = wfv1.LifecycleHooks{
				wfv1.ExitLifecycleEvent: {Template: "cleanup"},
			}
			workflow.Spec.Templates = append(workflow.Spec.Templates,
				wfv1.Template{Name: "cleanup", Container: &corev1.Container{Image: "alpine"}})
			hookID := appendRetryHook(workflow, "address")
			results := fixtureStepResults(workflow)
			check := stepByKey(t, results, "address/check")
			get := stepByKey(t, results, "address/get")
			if check.NodeID != "address-1-check" || get.NodeID != "address-1-get" || check.Status != stepStatus(final) {
				t.Fatalf("lost final retry rows: %+v", results)
			}
			hook := stepByKey(t, results, "cleanup/cleanup")
			if hook.NodeID != hookID || hook.Attempt != "" || hook.Superseded {
				t.Fatalf("hook treated as an attempt: %+v", hook)
			}
			completion := completionFor(workflow)
			if !completion.Succeeded || completion.FailedStage != "" || completion.FailedStep != "" {
				t.Fatalf("Argo Succeeded with tolerated retry failure acquired failed identity: %+v", completion)
			}
		})
	}
}

func TestRetryAttemptsRequireMatchingNameAndFlags(t *testing.T) {
	for _, test := range []struct {
		name string
		flag *wfv1.NodeFlag
		want bool
	}{
		{"wf.task(2)", nil, true},
		{"wf.task(2)", &wfv1.NodeFlag{Retried: true}, true},
		{"wf.task(2)", &wfv1.NodeFlag{Hooked: true, Retried: true}, true},
		{"wf.task(2)", &wfv1.NodeFlag{Hooked: true}, false},
		{"wf.task(2)", &wfv1.NodeFlag{}, true},
		{"wf.task.onExit", nil, false},
		{"wf.task.onExit", &wfv1.NodeFlag{Retried: true}, false},
		{"wf.other(99)", &wfv1.NodeFlag{Retried: true}, false},
	} {
		nodes := wfv1.Nodes{
			"retry":     {Name: "wf.task", Type: wfv1.NodeTypeRetry, Children: []string{"first", "candidate", "missing"}},
			"first":     {Name: "wf.task(1)"},
			"candidate": {Name: test.name, NodeFlag: test.flag},
		}
		retries := retryAttemptsOf(nodes)
		wantLatest := "first"
		if test.want {
			wantLatest = "candidate"
		}
		if retries.isAttempt("candidate") != test.want || retries.latest["retry"] != wantLatest || retries.isAttempt("missing") {
			t.Errorf("name %q flags %+v: attempts = %+v, want latest %s", test.name, test.flag, retries, wantLatest)
		}
	}
}
