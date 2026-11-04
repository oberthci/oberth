package argojob

import (
	"encoding/json"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func resourceAttemptInventory(wf *wfv1.Workflow, runID string) model.ResourceAttemptInventory {
	i := model.ResourceAttemptInventory{RunID: runID, WorkflowName: wf.Name, WorkflowUID: string(wf.UID), Namespace: wf.Namespace, Complete: wf.Status.CompressedNodes == "" && wf.Status.OffloadNodeStatusVersion == "" && len(wf.Status.Nodes) <= 2048, Terminal: wf.Status.Phase.Completed()}
	if len(wf.Status.Nodes) > 2048 {
		return i
	}
	mapped := map[string]bool{}
	var lock argoworkflow.Lock
	_ = json.Unmarshal([]byte(wf.Annotations[FragmentsAnnotation]), &lock)
	for _, step := range stepAttempts(wf) {
		node, ok := wf.Status.Nodes[step.NodeID]
		if !ok || node.Type != wfv1.NodeTypePod || node.Phase == wfv1.NodeSkipped || node.Phase == wfv1.NodeOmitted {
			continue
		}
		if node.ID != step.NodeID || step.AttemptAmbiguous || node.TemplateName == "" {
			i.Complete = false
			continue
		}
		a := model.ResourceAttempt{NodeID: node.ID, Stage: step.Stage, Step: step.Step, Template: node.TemplateName, Status: model.StepStatus(step.Status)}
		a.Owner, _ = argoworkflow.FragmentTemplateOwner(node.TemplateName, lock)
		check := i
		check.Attempts = []model.ResourceAttempt{a}
		if check.Validate() != nil {
			i.Complete = false
			continue
		}
		i.Attempts = append(i.Attempts, a)
		mapped[node.ID] = true
	}
	for id, node := range wf.Status.Nodes {
		if node.ID != id || (node.Type == wfv1.NodeTypePod && node.Phase != wfv1.NodeSkipped && node.Phase != wfv1.NodeOmitted && !mapped[id]) {
			i.Complete = false
		}
		if node.BoundaryID != "" {
			if _, ok := wf.Status.Nodes[node.BoundaryID]; !ok {
				i.Complete = false
			}
		}
		for _, references := range [][]string{node.Children, node.OutboundNodes} {
			for _, target := range references {
				if _, ok := wf.Status.Nodes[target]; !ok {
					i.Complete = false
				}
			}
		}
	}
	return i
}

func ownedResourcePod(wf *wfv1.Workflow, node wfv1.NodeStatus, pod *corev1.Pod) bool {
	owner := metav1.GetControllerOf(pod)
	return owner != nil && owner.APIVersion == "argoproj.io/v1alpha1" && owner.Kind == "Workflow" && owner.UID == wf.UID && owner.Name == wf.Name && pod.Namespace == wf.Namespace && pod.Annotations["workflows.argoproj.io/node-id"] == node.ID && pod.Annotations["workflows.argoproj.io/node-name"] == node.Name && pod.Spec.NodeName != ""
}
