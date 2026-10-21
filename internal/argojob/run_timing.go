package argojob

import (
	"context"
	"errors"
	"log"
	"path"
	"sort"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/internal/model"
)

type TimingRecorder interface {
	RecordRunTimingMilestone(context.Context, string, string) error
	RecordWorkflowTiming(context.Context, model.WorkflowTiming) error
	RecordStepTimings(context.Context, []model.StepTiming) error
}

func (controller *Controller) WithTimingRecorder(recorder TimingRecorder) *Controller {
	controller.timings = recorder
	return controller
}

func (controller *Controller) timingMilestone(ctx context.Context, runID, phase string) {
	if controller.timings == nil {
		return
	}
	if controller.timings.RecordRunTimingMilestone(ctx, runID, phase) != nil {
		log.Print("WARN: run phase timing observation unavailable")
	}
}

func (controller *Controller) retainWorkflowTiming(ctx context.Context, workflow *wfv1.Workflow) {
	if controller.timings == nil {
		return
	}
	runID, identity, ok := WorkflowMeta(workflow)
	if !ok {
		return
	}
	value := model.WorkflowTiming{RunID: runID, Namespace: workflow.Namespace, Name: workflow.Name, UID: string(workflow.UID), SpecIdentity: identity, CreatedAt: workflow.CreationTimestamp.Time, Tasks: []model.TimingTask{}}
	var err error
	value.Tasks, err = timingGraph(workflow)
	if err != nil {
		value.Tasks = []model.TimingTask{}
		value.GraphUnavailable = "unsupported-graph"
	}
	// An oversized/unsupported task projection must not discard the valid
	// Workflow binding needed to retain independently bounded Pod evidence.
	if err == nil && value.Validate() != nil {
		value.Tasks = []model.TimingTask{}
		value.GraphUnavailable = "graph-bound"
	}
	if controller.timings.RecordWorkflowTiming(ctx, value) != nil {
		log.Print("WARN: Workflow timing binding unavailable")
	}
}

// timingGraph walks the admitted expanded object, retaining only call identities
// and simple dependency names. Conditions and scripts are never serialized.
func timingGraph(workflow *wfv1.Workflow) ([]model.TimingTask, error) {
	templates := map[string]*wfv1.Template{}
	for i := range workflow.Spec.Templates {
		template := &workflow.Spec.Templates[i]
		templates[template.Name] = template
	}
	tasks := []model.TimingTask{}
	var walk func(*wfv1.Template, string, int) error
	walk = func(template *wfv1.Template, prefix string, depth int) error {
		if template == nil || depth > 32 {
			return errors.New("unsupported timing graph")
		}
		add := func(name, target string, inline *wfv1.Template, dependencies []string, mode string) error {
			if len(tasks) >= model.MaxTimingTasks {
				return errors.New("timing graph bound")
			}
			id := path.Join(prefix, name)
			resolved := templates[target]
			if inline != nil {
				resolved = inline
				target = name
			}
			qualified := make([]string, 0, len(dependencies))
			for _, dependency := range dependencies {
				qualified = append(qualified, path.Join(prefix, dependency))
			}
			tasks = append(tasks, model.TimingTask{ID: id, Template: target, Dependencies: qualified, Readiness: mode})
			if resolved == nil {
				return errors.New("unresolved timing template")
			}
			if mode != "dynamic" && (resolved.DAG != nil || len(resolved.Steps) > 0) {
				return walk(resolved, id, depth+1)
			}
			return nil
		}
		if template.DAG != nil {
			for _, task := range template.DAG.Tasks {
				mode := "all"
				if task.Depends != "" {
					mode = "expression-unavailable"
				}
				if task.When != "" {
					mode = "conditional"
				}
				if task.WithParam != "" || len(task.WithItems) > 0 || task.WithSequence != nil {
					mode = "dynamic"
				}
				if err := add(task.Name, task.Template, task.Inline, task.Dependencies, mode); err != nil {
					return err
				}
			}
		}
		previous := []string{}
		for _, group := range template.Steps {
			next := []string{}
			for _, step := range group.Steps {
				mode := "all"
				if step.When != "" {
					mode = "conditional"
				}
				if step.WithParam != "" || len(step.WithItems) > 0 || step.WithSequence != nil {
					mode = "dynamic"
				}
				if err := add(step.Name, step.Template, step.Inline, previous, mode); err != nil {
					return err
				}
				next = append(next, step.Name)
			}
			previous = next
		}
		return nil
	}
	if err := walk(templates[workflow.Spec.Entrypoint], "", 0); err != nil {
		return nil, err
	}
	return tasks, nil
}

// dependencyReady is logical DAG readiness, not a measured controller or image
// pull latency. Only an unambiguous conjunction of successful predecessors is
// projected; expressions, loops, guards and incomplete status stay unknown.
func dependencyReady(workflow *wfv1.Workflow, node wfv1.NodeStatus) *time.Time {
	boundary, ok := workflow.Status.Nodes[node.BoundaryID]
	if !ok {
		return nil
	}
	var template *wfv1.Template
	for i := range workflow.Spec.Templates {
		if workflow.Spec.Templates[i].Name == boundary.TemplateName {
			template = &workflow.Spec.Templates[i]
			break
		}
	}
	if template == nil || template.DAG == nil {
		return nil
	}
	for _, task := range template.DAG.Tasks {
		if task.Name != node.DisplayName {
			continue
		}
		if task.Depends != "" || task.When != "" || task.WithParam != "" || len(task.WithItems) > 0 || task.WithSequence != nil {
			return nil
		}
		ready := boundary.StartedAt.Time
		if ready.IsZero() {
			return nil
		}
		for _, dependency := range task.Dependencies {
			matches := 0
			for _, sibling := range workflow.Status.Nodes {
				if sibling.BoundaryID != node.BoundaryID || sibling.DisplayName != dependency {
					continue
				}
				matches++
				if sibling.Phase != wfv1.NodeSucceeded || sibling.FinishedAt.IsZero() {
					return nil
				}
				if sibling.FinishedAt.After(ready) {
					ready = sibling.FinishedAt.Time
				}
			}
			if matches != 1 {
				return nil
			}
		}
		return &ready
	}
	return nil
}

func projectStepTiming(workflow *wfv1.Workflow, node wfv1.NodeStatus, pod *corev1.Pod, step StepResult, diagnostic *model.StepDiagnostic, observed time.Time) *model.StepTiming {
	runID, _, _ := WorkflowMeta(workflow)
	value := &model.StepTiming{Version: model.TimingVersion, RunID: runID, Namespace: workflow.Namespace, WorkflowName: workflow.Name, WorkflowUID: string(workflow.UID), NodeID: step.NodeID, PodName: step.PodName, Burn: step.Burn, Step: step.Step, Ordinal: step.Ordinal, ObservedAt: observed, Init: []model.ContainerTiming{}, Missing: []string{}, ScriptTiming: "mixed", ImagePullTiming: "not-observed"}
	value.Unavailable = diagnostic.Unavailable
	if stepPodBound(workflow, node, pod) {
		value.PodUID = string(pod.UID)
		if !pod.CreationTimestamp.IsZero() {
			at := pod.CreationTimestamp.Time
			value.PodCreatedAt = &at
		}
		count := 0
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionTrue {
				count++
				at := condition.LastTransitionTime.Time
				if !at.IsZero() {
					value.ScheduledAt = &at
				}
			}
		}
		if count > 1 {
			value.ScheduledAt = nil
		}
		value.DependencyReadyAt = dependencyReady(workflow, node)
		if !node.FinishedAt.IsZero() {
			at := node.FinishedAt.Time
			value.CompletedAt = &at
		}
		if diagnostic.Available {
			value.Available = true
			value.Unavailable = ""
			main := diagnostic.Container
			value.Main = &model.ContainerTiming{Name: "main", ContainerID: main.ContainerID, RestartCount: main.RestartCount, StartedAt: main.StartedAt, FinishedAt: main.FinishedAt}
			seen := map[string]bool{}
			specs := map[string]bool{}
			for _, container := range pod.Spec.InitContainers {
				if specs[container.Name] {
					value.Available = false
					value.Unavailable = "unsupported-shape"
				}
				specs[container.Name] = true
			}
			for _, status := range pod.Status.InitContainerStatuses {
				if seen[status.Name] || !specs[status.Name] {
					value.Available = false
					value.Unavailable = "unsupported-shape"
					break
				}
				seen[status.Name] = true
				ended := status.State.Terminated
				if ended == nil || status.Ready || status.State.Running != nil || status.State.Waiting != nil || ended.ContainerID == "" || ended.ContainerID != status.ContainerID {
					continue
				}
				value.Init = append(value.Init, model.ContainerTiming{Name: status.Name, ContainerID: status.ContainerID, RestartCount: status.RestartCount, StartedAt: ended.StartedAt.Time, FinishedAt: ended.FinishedAt.Time})
			}
			if !value.Available {
				value.Main = nil
				value.Init = []model.ContainerTiming{}
			}
		}
	}
	sort.Slice(value.Init, func(i, j int) bool { return value.Init[i].Name < value.Init[j].Name })
	if value.DependencyReadyAt == nil {
		value.Missing = append(value.Missing, "dependency-ready")
	}
	if value.PodCreatedAt == nil {
		value.Missing = append(value.Missing, "pod-created")
	}
	if value.ScheduledAt == nil {
		value.Missing = append(value.Missing, "scheduled")
	}
	if pod == nil || len(value.Init) != len(pod.Spec.InitContainers) {
		value.Missing = append(value.Missing, "init")
	}
	if value.Main == nil {
		value.Missing = append(value.Missing, "main")
	}
	if value.CompletedAt == nil {
		value.Missing = append(value.Missing, "completed")
	}
	if value.Validate() != nil {
		value.Available = false
		value.Unavailable = "invalid-termination"
		value.Main = nil
		value.Init = []model.ContainerTiming{}
		value.DependencyReadyAt = nil
		value.PodCreatedAt = nil
		value.ScheduledAt = nil
		value.CompletedAt = nil
		value.Missing = []string{"dependency-ready", "pod-created", "scheduled", "init", "main", "completed"}
	}
	return value
}

func (controller *Controller) retainStepTimings(ctx context.Context, completion *Completion) {
	if controller.timings == nil {
		return
	}
	values := make([]model.StepTiming, 0, len(completion.Steps))
	for _, step := range completion.Steps {
		if step.Timing != nil && step.Timing.Validate() == nil {
			values = append(values, *step.Timing)
		}
	}
	if len(values) > 0 && controller.timings.RecordStepTimings(ctx, values) != nil {
		log.Print("WARN: step timing retention unavailable")
	}
}
