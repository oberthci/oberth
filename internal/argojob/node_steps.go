package argojob

import (
	"context"
	"reflect"
	"sort"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/internal/model"
)

// NodeStepResults projects each native status node exactly once. Argo owns
// names, phases, messages and timestamps; retry and loop nodes stay distinct.
// An absent Pod binding stays unknown, without reconstructing a Pod name.
func NodeStepResults(workflow *wfv1.Workflow, pods []*corev1.Pod) []model.StepResult {
	var err error
	workflow, err = nativeNodeWorkflow(context.Background(), workflow)
	if err != nil || workflow == nil {
		return nil
	}
	podNames := map[string]string{}
	for id, pod := range nativeStepPods(workflow, pods) {
		podNames[id] = pod.Name
	}
	for _, container := range StepContainers(workflow, pods) {
		podNames[container.NodeID] = container.PodName
	}
	runID, _, _ := WorkflowMeta(workflow)
	results := make([]model.StepResult, 0, len(workflow.Status.Nodes))
	for id, node := range workflow.Status.Nodes {
		scope, owner := node.GetTemplateScope()
		stage := workflow.Name
		if boundary, exists := workflow.Status.Nodes[node.BoundaryID]; exists {
			stage = boundary.DisplayName
			if stage == "" {
				stage = boundary.Name
			}
		}
		step := node.DisplayName
		if step == "" {
			step = node.Name
		}
		template := node.TemplateName
		if template == "" && node.TemplateRef != nil {
			template = node.TemplateRef.Template
		}
		exit := 0
		if nodeExitCodeKnown(node) {
			exit = exitCode(node)
		}
		results = append(results, model.StepResult{
			LocalTemplate: node.TemplateRef == nil && scope == wfv1.ResourceScopeLocal && (owner == "" || owner == workflow.Name),
			RunID:         runID, NodeID: id, DisplayName: node.DisplayName, Template: template,
			NodeType: string(node.Type), BoundaryID: node.BoundaryID,
			Phase: string(node.Phase), Message: model.SanitizeNodeMessage(node.Message), PodName: podNames[id],
			Stage: stage, Step: step, Status: model.StepStatus(stepStatus(node.Phase)), ExitCode: exit, ExitCodeUnknown: !nodeExitCodeKnown(node),
			StartedAt: optionalTime(node.StartedAt), FinishedAt: optionalTime(node.FinishedAt),
		})
	}
	sort.Slice(results, func(i, j int) bool {
		a, b := results[i], results[j]
		if a.StartedAt != nil && b.StartedAt == nil {
			return true
		}
		if a.StartedAt == nil && b.StartedAt != nil {
			return false
		}
		if a.StartedAt != nil && b.StartedAt != nil && !a.StartedAt.Equal(*b.StartedAt) {
			return a.StartedAt.Before(*b.StartedAt)
		}
		return a.NodeID < b.NodeID
	})
	for i := range results {
		results[i].Ordinal = i
	}
	return results
}

// StepResultWriter stores observed nodes in bounded writes, without a run cap.
type StepResultWriter interface {
	PutStepResults(context.Context, string, []model.StepResult) error
}

// WithStepResultWriter wires the history writer before serving runs.
func (controller *Controller) WithStepResultWriter(writer StepResultWriter) *Controller {
	controller.stepResults = writer
	return controller
}

// RecordNodeSteps consumes the same verified snapshot as the watch wait loop.
// Missing observations never clear existing rows or change the run verdict.
// recorded belongs to one wait invocation; failed writes remain eligible to retry.
func (controller *Controller) RecordNodeSteps(ctx context.Context, workflow *wfv1.Workflow, pods []*corev1.Pod, recorded map[string]model.StepResult) error {
	if controller.stepResults == nil {
		return nil
	}
	var err error
	workflow, err = nativeNodeWorkflow(ctx, workflow)
	if err != nil {
		return err
	}
	if workflow == nil {
		return nil
	}
	runID, _, _ := WorkflowMeta(workflow)
	rows := NodeStepResults(workflow, pods)
	changed := make([]model.StepResult, 0)
	for _, row := range rows {
		previous, exists := recorded[row.NodeID]
		if !exists || !reflect.DeepEqual(previous, row) {
			changed = append(changed, row)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	if err := controller.stepResults.PutStepResults(ctx, runID, changed); err != nil {
		return err
	}
	for _, row := range changed {
		recorded[row.NodeID] = row
	}
	return nil
}
