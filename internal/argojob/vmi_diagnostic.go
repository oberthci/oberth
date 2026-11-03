package argojob

import (
	"context"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
)

// VM failure evidence deliberately excludes free-form messages, manifests,
// addresses and guest data. Unknown reason/type values remain explicit "other".
type vmiDiagnostic struct {
	Name                  string         `json:"name"`
	UID                   string         `json:"uid"`
	Phase                 string         `json:"phase"`
	Conditions            []vmiCondition `json:"conditions"`
	ConditionsUnavailable string         `json:"conditions_unavailable,omitempty"`
	Events                []vmiEvent     `json:"events"`
	EventsUnavailable     string         `json:"events_unavailable,omitempty"`
}
type vmiCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}
type vmiEvent struct {
	Type   string    `json:"type"`
	Reason string    `json:"reason"`
	Count  int32     `json:"count"`
	At     time.Time `json:"at"`
}
type vmiSnapshot struct {
	Version     int             `json:"version"`
	WorkflowUID string          `json:"workflow_uid"`
	Burn        string          `json:"burn"`
	Step        string          `json:"step"`
	Unavailable string          `json:"unavailable,omitempty"`
	VMIs        []vmiDiagnostic `json:"vmis"`
}

func diagnosticEnum(value string, allowed ...string) string {
	for _, item := range allowed {
		if value == item {
			return value
		}
	}
	return "other"
}
func vmiReason(value string) string {
	return diagnosticEnum(value, "", "PodNotExists", "PodNotReady", "GuestNotRunning", "GuestAgentNotConnected", "VMINotExists", "VMINotMigratable", "DisksNotLiveMigratable", "FailedScheduling", "FailedMount", "FailedAttachVolume", "FailedCreate", "FailedDelete", "SyncFailed", "Synchronized", "PausedByUser", "PausedByIOError", "Unschedulable", "ImagePullBackOff", "ErrImagePull", "BackOff", "Created", "Started", "Scheduled", "SuccessfulCreate", "SuccessfulDelete", "Killing", "Provisioning", "ProvisioningCompleted")
}

// Failed address waits and resource deletes both terminate pipeline steps. Take
// the same generic, run-bound snapshot for failed VM workflows before cleanup.
// The total five-second budget and collection bounds also apply to API errors.
func (controller *Controller) collectVMIFailure(ctx context.Context, workflow *wfv1.Workflow, completion *Completion, destination io.Writer) {
	if completion.Succeeded || controller.vmi == nil || !hasVMIResources(workflow) {
		return
	}
	step := StepResult{Burn: completion.FailedBurn, Step: completion.FailedStep}
	if step.Burn == "" || step.Step == "" {
		return
	}
	value := vmiSnapshot{Version: 1, WorkflowUID: string(workflow.UID), Burn: step.Burn, Step: step.Step, VMIs: []vmiDiagnostic{}}
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	list, err := controller.vmi.Resource(vmiResource).Namespace(workflow.Namespace).List(readCtx, metav1.ListOptions{LabelSelector: "oberth.ci/workflow=" + workflow.Name, Limit: 8})
	if err != nil {
		value.Unavailable = "observation-unavailable"
	} else if list.GetContinue() != "" || len(list.Items) > 8 {
		value.Unavailable = "snapshot-incomplete"
	} else {
		for _, object := range list.Items {
			if object.GetNamespace() != workflow.Namespace || object.GetLabels()["oberth.ci/workflow"] != workflow.Name || !strings.HasPrefix(object.GetName(), workflow.Name+"-") || object.GetUID() == "" {
				continue
			}
			phase, _, _ := unstructured.NestedString(object.Object, "status", "phase")
			view := vmiDiagnostic{Name: object.GetName(), UID: string(object.GetUID()), Phase: diagnosticEnum(phase, "Pending", "Scheduling", "Scheduled", "Running", "Succeeded", "Failed", "Unknown", "WaitingForSync", "Unset"), Conditions: []vmiCondition{}, Events: []vmiEvent{}}
			conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
			if len(conditions) > 16 {
				view.ConditionsUnavailable = "snapshot-incomplete"
				conditions = conditions[:16]
			}
			for _, raw := range conditions {
				condition, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				kind, _, _ := unstructured.NestedString(condition, "type")
				status, _, _ := unstructured.NestedString(condition, "status")
				reason, _, _ := unstructured.NestedString(condition, "reason")
				view.Conditions = append(view.Conditions, vmiCondition{Type: diagnosticEnum(kind, "Ready", "LiveMigratable", "PodScheduled", "Synchronized", "Paused", "AgentConnected", "Initialized", "Provisioning", "CpuChange", "MemoryChange"), Status: diagnosticEnum(status, "True", "False", "Unknown"), Reason: vmiReason(reason)})
			}
			if controller.kube == nil {
				view.EventsUnavailable = "observation-unavailable"
			} else {
				selector := fields.Set{"involvedObject.uid": string(object.GetUID()), "involvedObject.kind": "VirtualMachineInstance", "involvedObject.name": object.GetName(), "involvedObject.namespace": workflow.Namespace}.AsSelector().String()
				events, eventErr := controller.kube.CoreV1().Events(workflow.Namespace).List(readCtx, metav1.ListOptions{FieldSelector: selector, Limit: 64})
				if eventErr != nil {
					view.EventsUnavailable = "observation-unavailable"
				} else {
					if events.Continue != "" || len(events.Items) > 64 {
						view.EventsUnavailable = "snapshot-incomplete"
					}
					for _, event := range events.Items[:min(len(events.Items), 64)] {
						ref := event.InvolvedObject
						if ref.UID != object.GetUID() || ref.Name != object.GetName() || ref.Namespace != workflow.Namespace || ref.Kind != "VirtualMachineInstance" || ref.APIVersion != "kubevirt.io/v1" {
							continue
						}
						at := event.LastTimestamp.Time
						if at.IsZero() {
							at = event.EventTime.Time
						}
						if at.IsZero() {
							at = event.CreationTimestamp.Time
						}
						view.Events = append(view.Events, vmiEvent{Type: diagnosticEnum(event.Type, corev1.EventTypeNormal, corev1.EventTypeWarning), Reason: vmiReason(event.Reason), Count: event.Count, At: at})
					}
					sort.Slice(view.Events, func(i, j int) bool { return view.Events[i].At.After(view.Events[j].At) })
					view.Events = view.Events[:min(len(view.Events), 16)]
				}
			}
			value.VMIs = append(value.VMIs, view)
		}
	}
	body, err := json.Marshal(value)
	if err != nil || len(body) > 64<<10 {
		return
	}
	completion.VMIDiagnostics = body
	if destination != nil {
		writeLine(destination, step, "oberth: VMI diagnostics "+string(body))
	}
}
