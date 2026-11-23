package argojob

import (
	"context"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/workflow/packer"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"
)

// Observation carries native facts; terminal recording belongs to the service.
type Observation struct {
	Steps          []stepmodel.RunStep
	Nodes          []stateport.Node
	Status         stepmodel.RunStatus
	Phase          stepmodel.NativePhase
	Reason, Detail string
	Failure        *stepmodel.FailureRecord
	Complete       bool
	workflow       *wf.Workflow
}
type projected struct {
	step  stepmodel.RunStep
	class stepmodel.FailureClass
}

// Project reads copies and folds retry nodes into the admitted logical paths.
type Projection struct {
	Plan          admissioncontract.Plan
	Workflow      *wf.Workflow
	Pods          []*core.Pod
	Previous      []stepmodel.RunStep
	PreviousNodes []stateport.Node
}

func Project(ctx context.Context, in Projection) (Observation, error) {
	if err := ctx.Err(); err != nil {
		return Observation{}, err
	}
	plan, w, pods, prev := in.Plan, in.Workflow.DeepCopy(), in.Pods, in.Previous
	if err := packer.DecompressWorkflow(ctx, w); err != nil {
		return Observation{}, err
	}
	out := Observation{workflow: w, Phase: stepmodel.NativePhase(w.Status.Phase), Detail: runDetail(w), Complete: len(w.Status.Nodes) > 0 && w.Status.OffloadNodeStatusVersion == ""}
	out.Status, out.Reason = runStatus(w)
	if !out.Phase.Valid() {
		out.Phase = stepmodel.NativeUnknown
	}
	groups, paths, err := groupNativeNodes(ctx, w)
	if err != nil {
		return Observation{}, err
	}
	rows := []projected{}
	waits := waitingLocks(w, paths)
	for _, entry := range plan.Entries {
		if err := ctx.Err(); err != nil {
			return Observation{}, err
		}
		rows = appendPlan(rows, projectionInput{entry: entry, w: w, pods: pods, groups: groups, out: &out, paths: paths, waits: waits, previous: prev, previousNodes: in.PreviousNodes})
	}
	seen := 0
	for _, nodes := range groups {
		seen += len(nodes)
	}
	if len(out.Nodes) != seen {
		out.Complete = false
	}
	mergeEvidence(&out, in.PreviousNodes)
	rows = mergeRows(rows, prev, plan, out.Nodes)
	for _, r := range rows {
		out.Steps = append(out.Steps, r.step)
	}
	out.Steps = stateport.PlanSteps(plan, out.Steps)
	finishProjection(&out, rows)
	if !terminalWorkflow(w) {
		out.Status, out.Reason = stepmodel.Progress(stepmodel.RunQueued, out.Steps, queuedReason(rows))
	}
	return out, nil
}

type projectionInput struct {
	entry         admissioncontract.PlanEntry
	w             *wf.Workflow
	pods          []*core.Pod
	groups        map[string][]wf.NodeStatus
	out           *Observation
	paths         nativePaths
	waits         map[string]string
	previous      []stepmodel.RunStep
	previousNodes []stateport.Node
}

func appendPlan(rows []projected, in projectionInput) []projected {
	paths := plannedPaths(in)
	for _, path := range paths {
		waiting := waitingFor(in.w, in.waits, path)
		row := projected{step: stepmodel.RunStep{Path: path, Name: in.entry.Name, Status: stepmodel.StepPending, DisplayReason: waiting}}
		if waiting != reasonWaiting && !terminalWorkflow(in.w) {
			row.step.Detail = waitingDetail(in.w, waiting)
		}
		nodes := in.groups[path]
		sort.Slice(nodes, func(i, j int) bool { return in.paths.less(nodes[i], nodes[j]) })
		for _, n := range nodes {
			projectAttempt(&row, in, n)
		}
		row.step.AttemptCount = len(nodes)
		if len(nodes) == 0 && terminalWorkflow(in.w) {
			row.step.Status, row.step.DisplayReason = unobservedStep(path, in.previous)
			in.out.Complete = false
		}
		rows = append(rows, row)
	}
	if in.entry.Dynamic && len(paths) == 0 {
		status, reason := stepmodel.StepPending, waitingFor(in.w, in.waits, in.entry.Path)
		if terminalWorkflow(in.w) {
			status, reason = stepmodel.StepSkipped, reasonNotReached
		}
		rows = append(rows, projected{step: stepmodel.RunStep{Path: in.entry.Path, Name: in.entry.Name, Status: status, DisplayReason: reason}})
		in.out.Complete = false
	}
	return rows
}
func plannedPaths(in projectionInput) []string {
	if !in.entry.Dynamic {
		return []string{in.entry.Path}
	}
	known := map[string]bool{}
	for path := range in.groups {
		if underPath(path, in.entry.Path) {
			known[path] = true
		}
	}
	for _, s := range in.previous {
		if s.Path != in.entry.Path && underPath(s.Path, in.entry.Path) {
			known[s.Path] = true
		}
	}
	return slices.Sorted(maps.Keys(known))
}
func projectAttempt(row *projected, in projectionInput, n wf.NodeStatus) {
	attempt := 1
	if order := in.paths[n.ID].order; len(order) > 0 {
		attempt = order[len(order)-1] + 1
	}
	for _, old := range in.previousNodes {
		if old.NodeID == n.ID {
			attempt = old.Attempt
		}
	}
	p := boundPod(in.w, n, in.pods)
	d := diagnose(n, p, in.w.Spec.Shutdown != "", nodeTemplate(in.w, n, admissioncontract.MaxNestingDepth))
	row.step.Status, row.step.DisplayReason, row.step.Detail, row.class = nodeStatus(n.Phase), d.reason, d.detail, d.class
	if n.SynchronizationStatus != nil && n.SynchronizationStatus.Waiting != "" {
		lock := lockReason(n.SynchronizationStatus.Waiting)
		row.step.DisplayReason = lock
		row.step.Detail = strings.TrimSpace(row.step.Detail + "\n" + detailLockStatus)
	}
	if row.step.StartedAt == nil {
		row.step.StartedAt = timePointer(n.StartedAt.Time)
	}
	row.step.FinishedAt = timePointer(n.FinishedAt.Time)
	podName := ""
	if p != nil {
		podName = p.Name
		row.step.Logs = append(row.step.Logs, stepmodel.LogSelector{NodeID: n.ID, PodName: p.Name, Container: d.container})
	}
	row.step.Attempts = append(row.step.Attempts, stepmodel.StepAttempt{NodeID: n.ID, PodName: podName, Status: row.step.Status})
	in.out.Nodes = append(in.out.Nodes, stateport.Node{NodeID: n.ID, Order: append([]int(nil), in.paths[n.ID].order...), Path: row.step.Path, Template: n.TemplateName, Boundary: n.BoundaryID, PodName: podName, Message: d.detail, Attempt: attempt, Status: row.step.Status, NeverRunSkip: neverRunSkip(n, in.pods), PodRestarted: n.FailedPodRestarts > 0, Exit: d.exit, Credential: new(in.entry.CredentialIntent), StartedAt: timePointer(n.StartedAt.Time), FinishedAt: timePointer(n.FinishedAt.Time)})
	if !terminalStep(row.step.Status) || d.exit == nil && n.Phase != wf.NodeSkipped && n.Phase != wf.NodeOmitted {
		in.out.Complete = false
	}
}

// runDetail is the native message; a run still pending on a workflow-level
// lock also points to lock status, as a step waiting for a lock does.
func runDetail(w *wf.Workflow) string {
	s := w.Status.Synchronization
	pending := w.Status.Phase == "" || w.Status.Phase == wf.WorkflowPending
	if !pending || s == nil || (s.Semaphore == nil || len(s.Semaphore.Waiting) == 0) && (s.Mutex == nil || len(s.Mutex.Waiting) == 0) {
		return w.Status.Message
	}
	return strings.TrimSpace(w.Status.Message + "\n" + detailLockStatus)
}

func runStatus(w *wf.Workflow) (stepmodel.RunStatus, string) {
	switch w.Status.Phase {
	case "", wf.WorkflowPending:
		return stepmodel.RunQueued, reasonWaiting
	case wf.WorkflowRunning:
		return stepmodel.RunRunning, reasonRunning
	case wf.WorkflowSucceeded:
		// Terminate releases a Workflow still pending on a workflow-level lock
		// by marking it Succeeded; its entry node never succeeded.
		if root, ok := w.Status.Nodes[w.Name]; w.Spec.Shutdown != "" && (!ok || root.Phase != wf.NodeSucceeded) {
			return stepmodel.RunCancelled, reasonStopped
		}
		return stepmodel.RunPassed, reasonPassed
	case wf.WorkflowFailed, wf.WorkflowError:
		if w.Spec.Shutdown != "" {
			return stepmodel.RunCancelled, reasonStopped
		}
		return stepmodel.RunFailed, reasonRunFailed
	default:
		return stepmodel.RunRunning, reasonUnknown
	}
}
func nodeStatus(p wf.NodePhase) stepmodel.StepStatus {
	switch p {
	case wf.NodePending:
		return stepmodel.StepQueued
	case wf.NodeRunning:
		return stepmodel.StepRunning
	case wf.NodeSucceeded:
		return stepmodel.StepPassed
	case wf.NodeFailed, wf.NodeError:
		return stepmodel.StepFailed
	case wf.NodeSkipped, wf.NodeOmitted:
		return stepmodel.StepSkipped
	default:
		return stepmodel.StepUnknown
	}
}
func executable(n wf.NodeStatus) bool {
	return n.Type == wf.NodeTypePod || n.Type == wf.NodeTypeSuspend || n.Type == wf.NodeTypeSkipped || n.Type == wf.NodeTypeHTTP || n.Type == wf.NodeTypePlugin
}
func timePointer(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
func terminalWorkflow(w *wf.Workflow) bool {
	return w.Status.Phase == wf.WorkflowSucceeded || w.Status.Phase == wf.WorkflowFailed || w.Status.Phase == wf.WorkflowError
}
func underPath(path, base string) bool {
	return base == "" || path == base || strings.HasPrefix(path, base+".") || strings.HasPrefix(path, base+"(") || strings.HasPrefix(path, base+"[")
}

func finishProjection(out *Observation, rows []projected) {
	out.Failure = primaryFailure(rows)
	if out.Status == stepmodel.RunCancelled && out.Failure != nil && out.Failure.Class != stepmodel.FailureCancelled {
		out.Status = stepmodel.RunFailed
	}
	if out.Status == stepmodel.RunPassed {
		out.Failure = nil
	}
	if out.Status == stepmodel.RunFailed && out.Failure != nil {
		for _, r := range rows {
			if r.step.Path == out.Failure.Step {
				out.Reason = r.step.DisplayReason
			}
		}
	}
}
func groupNativeNodes(ctx context.Context, w *wf.Workflow) (map[string][]wf.NodeStatus, nativePaths, error) {
	paths := indexPaths(w)
	groups := make(map[string][]wf.NodeStatus)
	for _, n := range w.Status.Nodes {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if executable(n) {
			path := paths[n.ID].name
			groups[path] = append(groups[path], n)
		}
	}
	return groups, paths, nil
}
func unobservedStep(path string, previous []stepmodel.RunStep) (stepmodel.StepStatus, string) {
	for _, s := range previous {
		if s.Path == path && s.AttemptCount > 0 {
			return stepmodel.StepUnknown, reasonUnknown
		}
	}
	return stepmodel.StepSkipped, reasonNotReached
}
