package argojob

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// StepResult is one executed pipeline step, in Oberth's vocabulary rather than
// Argo's. The engine derives it from the Workflow's node tree.
type StepResult struct {
	Timing               *model.StepTiming
	NodeID               string
	AttemptAmbiguous     bool
	Termination          *model.StepDiagnostic
	InfrastructureReason string
	Burn                 string
	Step                 string
	Ordinal              int
	Status               runprogress.Status
	ExitCode             int
	PodName              string
	StartedAt            *time.Time
	FinishedAt           *time.Time
	// Attempt is the display name of the Argo retry attempt that produced
	// this row (for example "check(1)" or "address(0)"), empty when no
	// retryStrategy encloses the step.
	Attempt string
	// Superseded marks a row from a retry attempt that a later attempt of
	// the same Retry node replaced. Such a row is evidence (its log and
	// progress stay in the run log) but never a verdict.
	Superseded bool
}

// Completion is the terminal outcome of one submitted Workflow.
type Completion struct {
	VMIDiagnostics []byte
	Name           string
	Succeeded      bool
	Infrastructure bool
	Phase          string
	Reason         string
	FailedBurn     string
	FailedStep     string
	Steps          []StepResult
}

// StepResults maps a Workflow's node tree onto Oberth's two-level burn/step
// model.
//
// The mapping is structural rather than conventional. Argo's node tree records
// each node's BoundaryID: the node ID of the template invocation that encloses
// it. A pod node's boundary is therefore the burn it belongs to whenever the
// pipeline nests a DAG (or steps) inside the entrypoint DAG -- which is the
// idiomatic Argo shape for "a group of steps that run together" and maps
// exactly onto a burn. A pod whose boundary is the root is a burn of one step.
//
// Nothing here requires the repository to adopt an Oberth-specific naming
// convention: the two-level shape falls out of the pipeline it already wrote.
func StepResults(workflow *wfv1.Workflow) []StepResult {
	return finalStepResults(stepAttempts(workflow))
}

func finalStepResults(observed []StepResult) []StepResult {
	if observed == nil {
		return nil
	}
	results := make([]StepResult, 0, len(observed))
	for _, result := range observed {
		// A superseded retry attempt is not the step's outcome: the Retry
		// node's later attempt is. Dropping it here is what keeps a failed
		// first attempt from failing a run whose retry passed, while the
		// attempt's own progress and log remain in the run log.
		if result.Superseded {
			continue
		}
		results = append(results, result)
	}
	// Deduplicate any remaining same-named rows (a Retry node whose attempt
	// order could not be read): keep the last-started one, so the durable
	// step result still reflects the terminal outcome.
	results = deduplicateRetries(results)
	for index := range results {
		results[index].Ordinal = index
	}
	return results
}

// stepAttempts maps every step node, including superseded retry attempts, in
// start order. Retry suffixes are stripped from both the step and its burn, so
// every attempt of one planned step carries that step's planned name.
func stepAttempts(workflow *wfv1.Workflow) []StepResult {
	if workflow == nil || len(workflow.Status.Nodes) == 0 {
		return nil
	}
	rootID := ""
	for id, node := range workflow.Status.Nodes {
		if node.Name == workflow.Name {
			rootID = id
			break
		}
	}
	retries := retryAttemptsOf(workflow.Status.Nodes)
	results := make([]StepResult, 0, len(workflow.Status.Nodes))
	for _, node := range workflow.Status.Nodes {
		if !stepNode(workflow, node) {
			continue
		}
		burn, step := burnAndStep(workflow.Status.Nodes, node, rootID, retries)
		if burn == "" || step == "" {
			continue
		}
		podName := ""
		if node.Type == wfv1.NodeTypePod {
			podName = PodName(workflow, node)
		}
		attempt, superseded := retries.attempt(workflow.Status.Nodes, node, rootID)
		results = append(results, StepResult{
			InfrastructureReason: nodeInfrastructureReason(node),
			NodeID:               node.ID,
			Attempt:              attempt,
			Superseded:           superseded,
			Burn:                 burn,
			Step:                 step,
			Status:               stepStatus(node.Phase),
			ExitCode:             exitCode(node),
			PodName:              podName,
			StartedAt:            optionalTime(node.StartedAt),
			FinishedAt:           optionalTime(node.FinishedAt),
		})
	}
	// Deterministic order: start time, then burn, then step, then node ID.
	// Parallel steps that started in the same instant still order stably.
	sort.Slice(results, func(left, right int) bool {
		leftStart, rightStart := results[left].StartedAt, results[right].StartedAt
		switch {
		case leftStart == nil && rightStart != nil:
			return false
		case leftStart != nil && rightStart == nil:
			return true
		case leftStart != nil && rightStart != nil && !leftStart.Equal(*rightStart):
			return leftStart.Before(*rightStart)
		}
		if results[left].Burn != results[right].Burn {
			return results[left].Burn < results[right].Burn
		}
		if results[left].Step != results[right].Step {
			return results[left].Step < results[right].Step
		}
		return results[left].NodeID < results[right].NodeID
	})
	for index := range results {
		results[index].Ordinal = index
	}
	return results
}

// stepNode reports whether a node is one of the pipeline's steps.
//
// A Pod node always is. A Skipped node is too when the template it would have
// run is one that produces a Pod: Argo creates the node for a task whose
// `when` was false or whose `depends` was never satisfied with
// Type: NodeTypeSkipped, never NodeTypePod, so discarding every non-Pod node
// meant a gate a repository authored -- a scan, a signature check -- could be
// removed from a green run leaving no trace anywhere. It is now reported as
// skipped, which is what stepStatus and wireBurns have always been able to
// render.
//
// The template check is what keeps this from inventing rows: Argo also skips
// the invocation node of a whole nested DAG, and that node is a burn, not a
// step. An inline template has an empty TemplateName; for those, a childless
// leaf node is accepted as a step (it would have been a single pod), while a
// node with children is a DAG/steps wrapper that is a burn boundary.
func stepNode(workflow *wfv1.Workflow, node wfv1.NodeStatus) bool {
	switch node.Type {
	case wfv1.NodeTypePod:
		return true
	case wfv1.NodeTypeSkipped:
		if podTemplate(workflow, node.TemplateName) {
			return true
		}
		if strings.TrimSpace(node.TemplateName) == "" && len(node.Children) == 0 {
			return true
		}
		return false
	default:
		return false
	}
}

func podTemplate(workflow *wfv1.Workflow, name string) bool {
	if strings.TrimSpace(name) == "" {
		return false
	}
	for index := range workflow.Spec.Templates {
		template := &workflow.Spec.Templates[index]
		if template.Name != name {
			continue
		}
		return template.Container != nil || template.Script != nil || template.ContainerSet != nil
	}
	return false
}

// PodNameVersionAnnotation and podNameVersionV2 pin how Argo names the Pod
// backing a node.
//
// The name is not the node ID: under the v2 convention (the default since Argo
// 3.5) it is "<workflow>-<template>-<fnv32a(nodeName)>". Oberth pins the
// annotation on every submission so the server's computation and the
// controller's cannot diverge with the controller's POD_NAMES environment --
// which would otherwise silently break step log capture, exactly as it did the
// first time this ran against a real cluster.
const (
	PodNameVersionAnnotation = "workflows.argoproj.io/pod-name-format"
	podNameVersionV2         = "v2"

	maxPodNamePrefixLength = 253 - 10 - 1
)

// PodName reproduces Argo's own deterministic Pod naming
// (workflow/util.GeneratePodName). It is reimplemented rather than imported
// because that package pulls in the whole controller.
// TestPodNameMatchesArgo pins it against the upstream algorithm, and the live
// suite proves it against a real controller.
func PodName(workflow *wfv1.Workflow, node wfv1.NodeStatus) string {
	if workflow.Annotations[PodNameVersionAnnotation] == "v1" {
		return node.ID
	}
	if workflow.Name == node.Name {
		return workflow.Name
	}
	prefix := workflow.Name
	if node.TemplateName != "" {
		prefix += "-" + node.TemplateName
	}
	if len(prefix) > maxPodNamePrefixLength {
		prefix = prefix[:maxPodNamePrefixLength]
	}
	digest := fnv.New32a()
	_, _ = digest.Write([]byte(node.Name))
	return fmt.Sprintf("%s-%v", prefix, digest.Sum32())
}

// burnAndStep names one observed node, deferring to the same rule the static
// enumeration applies to the document (argoworkflow.BurnAndStep). The only
// work done here is turning Argo's node tree into that rule's two inputs: the
// nearest enclosing non-root boundary, and the node itself.
//
// Either input can be an attempt of a Retry node: Argo names the attempts of a
// template with retryStrategy "task(0)", "task(1)", and so on. A retried pod
// is such an attempt itself; a retried DAG or steps template makes the
// attempt the boundary of every pod inside it. The suffix is stripped in both
// cases so the result resolves against the planned name from the document,
// which never carries one.
func burnAndStep(nodes wfv1.Nodes, node wfv1.NodeStatus, rootID string, retries retryAttempts) (string, string) {
	enclosing := ""
	if boundary, ok := nodes[node.BoundaryID]; ok && node.BoundaryID != "" && node.BoundaryID != rootID {
		enclosing = displayName(boundary)
		if retries.isAttempt(boundary.ID) {
			enclosing = stripRetrySuffix(enclosing)
		}
	}
	name := displayName(node)
	if retries.isAttempt(node.ID) {
		name = stripRetrySuffix(name)
	}
	return argoworkflow.BurnAndStep(enclosing, name)
}

func displayName(node wfv1.NodeStatus) string {
	name := strings.TrimSpace(node.DisplayName)
	if name == "" {
		name = strings.TrimSpace(node.Name)
	}
	if name == "" {
		name = strings.TrimSpace(node.TemplateName)
	}
	// Progress markers reject control characters and empty names.
	return argoworkflow.StepName(name)
}

// stripRetrySuffix removes Argo's retry attempt suffix from a step name.
//
// When a template has retryStrategy, the controller wraps the pod in a Retry
// node and names the child "task(0)" for the first attempt, "(1)" for the
// second, and so on. Stripping the suffix lets the step result match the
// planned step name, which carries no suffix because the plan is derived from
// the reviewed document. Without this, every retryStrategy template seeds a
// phantom pending row that the real pod result -- named with the suffix --
// never resolves.
func stripRetrySuffix(name string) string {
	if !strings.HasSuffix(name, ")") {
		return name
	}
	open := strings.LastIndex(name, "(")
	if open < 1 {
		return name
	}
	between := name[open+1 : len(name)-1]
	if between == "" {
		return name
	}
	for _, ch := range between {
		if ch < '0' || ch > '9' {
			return name
		}
	}
	return name[:open]
}

// retryAttempts indexes Argo's Retry nodes: which node is an attempt of which
// Retry node, and which attempt is each Retry node's latest.
type retryAttempts struct {
	parent map[string]string
	latest map[string]string
}

// retryAttemptsOf collects every Retry node's attempts. The latest attempt is
// the one with the highest Argo attempt index ("name(N)"); the order of the
// Retry node's Children breaks ties. Hooks and children without the exact
// retry attempt name are not attempts, regardless of their list position.
func retryAttemptsOf(nodes wfv1.Nodes) retryAttempts {
	retries := retryAttempts{parent: map[string]string{}, latest: map[string]string{}}
	for id, node := range nodes {
		if node.Type != wfv1.NodeTypeRetry {
			continue
		}
		latest, latestIndex := "", -1
		for _, child := range node.Children {
			attempt, ok := nodes[child]
			if !ok || child == "" || (attempt.NodeFlag != nil && attempt.NodeFlag.Hooked) {
				continue
			}
			// Argo flags attempts as Retried and hooks as Hooked. Older
			// snapshots may lack flags, but must still have an attempt name.
			index, ok := retryAttemptIndex(attempt.Name)
			if !ok || stripRetrySuffix(attempt.Name) != node.Name {
				continue
			}
			retries.parent[child] = id
			if index >= latestIndex {
				latest, latestIndex = child, index
			}
		}
		if latest != "" {
			retries.latest[id] = latest
		}
	}
	return retries
}

func (retries retryAttempts) isAttempt(id string) bool {
	_, ok := retries.parent[id]
	return ok
}

// attempt walks from a step node out through its enclosing boundaries to the
// root. It reports the innermost retry attempt the step ran in, and whether
// any enclosing Retry node has since started a later attempt -- which makes
// this row a superseded attempt rather than the step's outcome. Nested
// retries compose: a pod in the latest attempt of an inner Retry node is
// still superseded when the outer retried DAG has moved on to a new attempt.
func (retries retryAttempts) attempt(nodes wfv1.Nodes, node wfv1.NodeStatus, rootID string) (string, bool) {
	attempt, superseded := "", false
	current := node
	for depth := 0; depth <= len(nodes); depth++ {
		if retry, ok := retries.parent[current.ID]; ok {
			if attempt == "" {
				attempt = displayName(current)
			}
			if retries.latest[retry] != current.ID {
				superseded = true
			}
		}
		next, ok := nodes[current.BoundaryID]
		if !ok || current.BoundaryID == "" || current.BoundaryID == rootID || next.ID == current.ID {
			break
		}
		current = next
	}
	return attempt, superseded
}

// retryAttemptIndex parses the attempt index Argo appends to a Retry node's
// child: "wf.task(3)" is attempt 3.
func retryAttemptIndex(name string) (int, bool) {
	stripped := stripRetrySuffix(name)
	if stripped == name {
		return 0, false
	}
	index := 0
	for _, digit := range name[len(stripped)+1 : len(name)-1] {
		index = index*10 + int(digit-'0')
		if index > 1<<20 {
			return 0, false
		}
	}
	return index, true
}

// deduplicateRetries collapses multiple rows for the same burn+step into a
// single row carrying the last-started one's data. Superseded retry attempts
// are already gone by the time it runs; this is the fallback for a Retry node
// whose attempts could not be told apart. The input
// must be sorted by start time so that later attempts appear after earlier
// ones; the deduplicated row keeps the chronological position of the first
// attempt (the step's natural place in the pipeline) but uses the final
// attempt's status, exit code, pod name, and timestamps.
func deduplicateRetries(results []StepResult) []StepResult {
	type key struct{ burn, step string }
	lastIndex := make(map[key]int, len(results))
	for i, r := range results {
		lastIndex[key{r.Burn, r.Step}] = i
	}
	if len(lastIndex) == len(results) {
		return results
	}
	deduped := make([]StepResult, 0, len(lastIndex))
	emitted := make(map[key]bool, len(lastIndex))
	for _, r := range results {
		k := key{r.Burn, r.Step}
		if emitted[k] {
			continue
		}
		emitted[k] = true
		selected := results[lastIndex[k]]
		for _, candidate := range results {
			if candidate.Burn != selected.Burn || candidate.Step != selected.Step || candidate.NodeID == selected.NodeID {
				continue
			}
			if selected.StartedAt == nil || candidate.StartedAt == nil || selected.StartedAt.Equal(*candidate.StartedAt) {
				selected.AttemptAmbiguous = true
			}
		}
		deduped = append(deduped, selected)
	}
	return deduped
}

// terminal reports whether a step status is final. runprogress keeps its own
// predicate unexported, so the engine carries its own rather than widening a
// shared package's API for one caller.
func terminal(status runprogress.Status) bool {
	switch status {
	case runprogress.StepPassed, runprogress.StepFailed,
		runprogress.StepSkipped, runprogress.StepTimedOut:
		return true
	default:
		return false
	}
}

// stepStatus maps Argo's node phases onto Oberth's step vocabulary. Argo has
// two phases with no Oberth equivalent: Skipped (a `when` guard was false) and
// Omitted (a `depends` expression was not satisfied). Both mean "did not run
// and that is a normal outcome", which is exactly Oberth's skipped.
func stepStatus(phase wfv1.NodePhase) runprogress.Status {
	switch phase {
	case wfv1.NodePending:
		return runprogress.StepQueued
	case wfv1.NodeRunning:
		return runprogress.StepRunning
	case wfv1.NodeSucceeded:
		return runprogress.StepPassed
	case wfv1.NodeSkipped, wfv1.NodeOmitted:
		return runprogress.StepSkipped
	case wfv1.NodeFailed, wfv1.NodeError:
		return runprogress.StepFailed
	default:
		return runprogress.StepQueued
	}
}

// exitCode reports the step's process exit code when Argo captured one. Argo
// records it as a string output field; a node that failed without one (an
// infrastructure error, an evicted pod) reports 1 so a failure is never
// recorded as a success.
func exitCode(node wfv1.NodeStatus) int {
	if node.Outputs != nil && node.Outputs.ExitCode != nil {
		if code, err := parseExitCode(*node.Outputs.ExitCode); err == nil {
			return code
		}
	}
	switch node.Phase {
	case wfv1.NodeSucceeded, wfv1.NodeSkipped, wfv1.NodeOmitted:
		return 0
	case wfv1.NodePending, wfv1.NodeRunning:
		return 0
	default:
		return 1
	}
}

func parseExitCode(value string) (int, error) {
	code := 0
	if value == "" {
		return 0, errEmptyExitCode
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, errEmptyExitCode
		}
		code = code*10 + int(character-'0')
		if code > 255 {
			return 255, nil
		}
	}
	return code, nil
}

func optionalTime(value metav1.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	moment := value.UTC()
	return &moment
}

// completionFor maps a terminal Workflow onto the engine's Completion.
//
// Completion success requires BOTH a Succeeded Workflow AND zero failed or
// errored configured step results. A step with retry attempts where the final
// attempt succeeded is a pass: StepResults drops superseded attempts, whether
// the retryStrategy wraps the pod itself or a whole DAG or steps template, so
// only the latest attempt of every Retry node is a verdict. Retries exhausted
// leave that latest attempt failed, which keeps the run red and attributes
// FailedBurn/FailedStep to it.
// A terminally failed configured step — regardless of whether the DAG's depends
// expressions handle it — makes the run red. Enhanced-depends cleanup or
// verification tasks may still run after a failure but must not convert the
// terminal run to green.
//
// The Workflow phase itself is never overridden upward: Argo's Failed phase
// always fails the run, whatever the step rows say. Steps come from
// Status.Nodes, and a task that never reached pod creation has no node row at
// all, so "no failed rows" cannot distinguish "all work passed" from "work
// never ran" (stopped, deadline-exceeded, or controller failure).
func completionFor(workflow *wfv1.Workflow) Completion {
	completion := Completion{
		Name:      workflow.Name,
		Phase:     string(workflow.Status.Phase),
		Reason:    strings.TrimSpace(workflow.Status.Message),
		Succeeded: workflow.Status.Phase == wfv1.WorkflowSucceeded,
		Steps:     StepResults(workflow),
	}

	for _, step := range completion.Steps {
		if step.Status == runprogress.StepPassed || step.Status == runprogress.StepSkipped {
			continue
		}
		if !terminal(step.Status) {
			continue
		}
		// Any terminally failed configured step is attributed as the
		// run-level failure. The DAG's depends expressions control Argo's
		// execution flow but do not control Oberth's success gate.
		completion.FailedBurn, completion.FailedStep = step.Burn, step.Step
		completion.Succeeded = false
	}

	if !completion.Succeeded && completion.Reason == "" {
		completion.Reason = "Workflow " + strings.ToLower(completion.Phase)
	}
	completion.classifyInfrastructure()
	return completion
}
