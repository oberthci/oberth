package argojob

import (
	"context"
	"errors"
	"sort"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/internal/model"
)

// maxSyncKeys bounds the lock keys one Workflow observation returns.
const maxSyncKeys = 32

// ObserveSynchronization reads one owned Workflow's native Argo
// synchronization status (#1514): the semaphore and mutex keys it holds and
// waits for. It reads the shared cache and never mutates anything.
// Keys keep the controller's "<ns>/<Kind>/<name>[/<key>]" form; the service
// strips the namespace. A completed or missing Workflow holds nothing. The
// Argo controller's parallelism appears as model.SyncParallelismLock.
func (controller *Controller) ObserveSynchronization(_ context.Context, name, runID string) (*model.SyncObservation, error) {
	if controller.hub == nil || !controller.hub.Healthy() {
		return nil, errWatchUnavailable
	}
	workflow, found := controller.hub.Workflow(name)
	if !found {
		return nil, errWatchUnavailable
	}
	if workflow.Name != name || workflow.Namespace != controller.config.Namespace || workflow.Annotations[runIDAnnotation] != runID {
		return nil, errors.New("argojob: synchronization observation identity mismatch")
	}
	observation := &model.SyncObservation{Holding: []string{}, Waiting: []string{}}
	if workflow.Status.Phase.Completed() {
		return observation, nil
	}
	holding, waiting := map[string]bool{}, map[string]bool{}
	// Argo controller parallelism (#1517) is reported like a lock: a
	// postponed Workflow waits for a slot; a Workflow the throttler admitted
	// (running, or Pending behind a workflow-level lock) holds one.
	switch {
	case postponedByParallelism(workflow):
		waiting[model.SyncParallelismLock] = true
	case workflow.Status.Phase == wfv1.WorkflowRunning ||
		(workflow.Status.Phase == wfv1.WorkflowPending && waitingForLock(workflow.Status.Message)):
		holding[model.SyncParallelismLock] = true
	}
	if workflow.Status.Synchronization == nil {
		observation.Holding, observation.Waiting = boundedKeys(holding), boundedKeys(waiting)
		return observation, nil
	}
	if semaphore := workflow.Status.Synchronization.Semaphore; semaphore != nil {
		for _, entry := range semaphore.Holding {
			holding[entry.Semaphore] = true
		}
		for _, entry := range semaphore.Waiting {
			waiting[entry.Semaphore] = true
		}
	}
	if mutex := workflow.Status.Synchronization.Mutex; mutex != nil {
		for _, entry := range mutex.Holding {
			holding[entry.Mutex] = true
		}
		for _, entry := range mutex.Waiting {
			waiting[entry.Mutex] = true
		}
	}
	// A key both held (by one template) and waited for (by another node)
	// is reported in both lists; the history keeps both roles apart.
	observation.Holding = boundedKeys(holding)
	observation.Waiting = boundedKeys(waiting)
	return observation, nil
}

func boundedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		if key != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) > maxSyncKeys {
		keys = keys[:maxSyncKeys]
	}
	return keys
}
