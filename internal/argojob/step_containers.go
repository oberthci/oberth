package argojob

import (
	"sort"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// StepContainer identifies a native Pod container's Argo node. Callers verify
// the Workflow's run identity before using this mapping to read its logs.
type StepContainer struct {
	NodeID        string
	Namespace     string
	PodName       string
	ContainerName string
	PodUID        types.UID
}

// StepContainers maps observed Pods and their containers to Argo node IDs.
// Pod names are opaque. Foreign or ambiguous Pod ownership never binds a node.
// Inputs may be shared watch-cache pointers and are never changed.
func StepContainers(workflow *wfv1.Workflow, pods []*corev1.Pod) []StepContainer {
	if workflow == nil || workflow.UID == "" {
		return nil
	}
	bound := nativeStepPods(workflow, pods)
	// ContainerSet nodes name their Pod node as BoundaryID. Other containers
	// (including init containers and sidecars) belong to that Pod's node.
	children := map[string]map[string][]string{}
	for id, node := range workflow.Status.Nodes {
		if node.Type != wfv1.NodeTypeContainer {
			continue
		}
		if children[node.BoundaryID] == nil {
			children[node.BoundaryID] = map[string][]string{}
		}
		children[node.BoundaryID][node.DisplayName] = append(children[node.BoundaryID][node.DisplayName], id)
	}
	var result []StepContainer
	for id, pod := range bound {
		appendContainer := func(name string, init bool) {
			nodeID := id
			if !init {
				matches := children[id][name]
				if len(matches) > 1 {
					return
				}
				if len(matches) == 1 {
					nodeID = matches[0]
				}
			}
			result = append(result, StepContainer{NodeID: nodeID, Namespace: pod.Namespace, PodName: pod.Name, ContainerName: name, PodUID: pod.UID})
		}
		for _, container := range pod.Spec.InitContainers {
			appendContainer(container.Name, true)
		}
		for _, container := range pod.Spec.Containers {
			appendContainer(container.Name, false)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].NodeID != result[j].NodeID {
			return result[i].NodeID < result[j].NodeID
		}
		return result[i].ContainerName < result[j].ContainerName
	})
	return result
}

func nativeStepPods(workflow *wfv1.Workflow, pods []*corev1.Pod) map[string]*corev1.Pod {
	bound := map[string]*corev1.Pod{}
	if workflow == nil || workflow.UID == "" {
		return bound
	}
	ambiguous := map[string]bool{}
	for _, pod := range pods {
		if pod == nil || pod.UID == "" || pod.Namespace != workflow.Namespace {
			continue
		}
		id := pod.Annotations["workflows.argoproj.io/node-id"]
		node, exists := workflow.Status.Nodes[id]
		if !exists || id == "" || node.ID != id || node.Type != wfv1.NodeTypePod || pod.Annotations["workflows.argoproj.io/node-name"] != node.Name {
			continue
		}
		owners, owned := 0, true
		for _, owner := range pod.OwnerReferences {
			if owner.Controller != nil && *owner.Controller {
				owners++
				owned = owned && owner.APIVersion == "argoproj.io/v1alpha1" && owner.Kind == "Workflow" && owner.Name == workflow.Name && owner.UID == workflow.UID
			}
		}
		if owners != 1 || !owned || ambiguous[id] {
			continue
		}
		if previous := bound[id]; previous != nil {
			if previous.Name == pod.Name && previous.UID == pod.UID {
				continue
			}
			delete(bound, id)
			ambiguous[id] = true
			continue
		}
		bound[id] = pod
	}
	return bound
}
