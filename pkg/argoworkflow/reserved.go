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

// ReleaseMutexName is the fixed native Argo mutex the server adds to every
// release run while release serialization is on. A document that took it
// itself would hold every release in the installation behind its own run.
const ReleaseMutexName = "oberth-release"

// admitWorkflowMetadata refuses Oberth-owned keys in spec.workflowMetadata.
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

// admitSynchronization refuses a document-declared lock on the release mutex
// the server reserves. Every other mutex name and every semaphore, including
// the oberth-sync-limits ConfigMap keys repositories are meant to share,
// remains the document's own choice.
func admitSynchronization(location string, synchronization *wfv1.Synchronization) []error {
	if synchronization == nil {
		return nil
	}
	var problems []error
	for index, mutex := range synchronization.Mutexes {
		if mutex == nil {
			continue
		}
		if strings.TrimSpace(mutex.Name) == ReleaseMutexName {
			problems = append(problems, fmt.Errorf(
				"argoworkflow: %s.mutexes[%d].name %q is reserved: Oberth adds it to every release run to serialize releases across the installation; use a mutex name of your own",
				location, index, mutex.Name))
		}
	}
	return problems
}
