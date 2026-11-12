package argoworkflow

import (
	"fmt"
	"sort"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

// ReservedMetadataPrefix is the label and annotation namespace Oberth owns on
// the Workflow object. The server writes the run's identity under it (run ID,
// repository, ref, trigger) and reads it back to prove ownership before it
// cancels, adopts or finalizes a Workflow.
const ReservedMetadataPrefix = "oberth.ci/"

// ArgoMetadataPrefix is the label and annotation namespace the Argo
// controller owns. workflowMetadata copies it onto the live Workflow, so a
// key such as workflows.argoproj.io/controller-instanceid there could hide
// the run from the controller Oberth submitted it to.
const ArgoMetadataPrefix = "workflows.argoproj.io/"

// templateTagOpen starts every Argo template tag, simple or expression.
const templateTagOpen = "{{"

// admitWorkflowMetadata refuses Oberth- and Argo-owned keys in
// spec.workflowMetadata.
// The Argo controller copies workflowMetadata onto the live Workflow at every
// reconcile, after Oberth has stamped its own metadata, so an oberth.ci/ key
// here would overwrite the server's ownership record. Keys outside the
// reserved namespace stay the document's business.
func admitWorkflowMetadata(location string, metadata *wfv1.WorkflowMetadata) []error {
	if metadata == nil {
		return nil
	}
	var problems []error
	refuse := func(field string, keys []string) {
		sort.Strings(keys)
		for _, key := range keys {
			if strings.HasPrefix(key, ReservedMetadataPrefix) {
				problems = append(problems, fmt.Errorf(
					"argoworkflow: %s.%s[%q] is refused: keys under %s are assigned by Oberth and record which run owns the Workflow; remove the key or rename it outside %s",
					location, field, key, ReservedMetadataPrefix, ReservedMetadataPrefix))
				continue
			}
			if strings.HasPrefix(key, ArgoMetadataPrefix) {
				problems = append(problems, fmt.Errorf(
					"argoworkflow: %s.%s[%q] is refused: keys under %s are assigned by the Argo controller and decide which controller runs the Workflow; remove the key or rename it outside %s",
					location, field, key, ArgoMetadataPrefix, ArgoMetadataPrefix))
				continue
			}
			// The controller substitutes {{...}} across the whole serialized
			// spec, map keys included, before it copies workflowMetadata, so
			// a templated key can resolve to a reserved one at run time.
			if strings.Contains(key, templateTagOpen) {
				problems = append(problems, fmt.Errorf(
					"argoworkflow: %s.%s[%q] is refused: a templated key could resolve to one under %s, which Oberth assigns; write the key literally",
					location, field, key, ReservedMetadataPrefix))
			}
		}
	}
	refuse("labels", mapKeys(metadata.Labels))
	refuse("annotations", mapKeys(metadata.Annotations))
	labelsFrom := make([]string, 0, len(metadata.LabelsFrom))
	for key := range metadata.LabelsFrom {
		labelsFrom = append(labelsFrom, key)
	}
	refuse("labelsFrom", labelsFrom)
	return problems
}

func mapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

// SyncLimitsConfigMap is the one ConfigMap a semaphore may read its limit
// from: the shared limits the installation keeps for repositories.
const SyncLimitsConfigMap = "oberth-sync-limits"

// admitSynchronization refuses a document-declared lock on the release mutex
// the server reserves, and a semaphore whose limit is read from anywhere but
// the oberth-sync-limits ConfigMap in the Workflow's namespace: the
// controller copies an unparsable value into the Workflow's status message,
// so any other ConfigMap key would be readable through a run. Every other
// mutex name and every oberth-sync-limits key remains the document's choice.
func admitSynchronization(location string, synchronization *wfv1.Synchronization) []error {
	if synchronization == nil {
		return nil
	}
	var problems []error
	for index, semaphore := range synchronization.Semaphores {
		if semaphore == nil {
			continue
		}
		if semaphore.Database != nil || semaphore.Namespace != "" || semaphore.ConfigMapKeyRef == nil || semaphore.ConfigMapKeyRef.Name != SyncLimitsConfigMap {
			problems = append(problems, fmt.Errorf(
				"argoworkflow: %s.semaphores[%d] is refused: a semaphore reads its limit from configMapKeyRef name %s, key of your choice, in the Workflow's own namespace",
				location, index, SyncLimitsConfigMap))
		}
	}
	return problems
}
