package argojob

import (
	"context"
	"errors"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/model"
)

// maxSyncKeys bounds the lock keys one Workflow observation returns.
const maxSyncKeys = 32

// ObserveSynchronization reads one owned Workflow's native Argo
// synchronization status (#1514): the semaphore and mutex keys it holds and
// waits for. It performs one exact Workflow GET and never mutates anything.
// Keys keep the controller's "<ns>/<Kind>/<name>[/<key>]" form; the service
// strips the namespace. A completed or missing Workflow holds nothing.
func (controller *Controller) ObserveSynchronization(ctx context.Context, name, runID string) (*model.SyncObservation, error) {
	workflow, err := controller.workflows.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if workflow.Name != name || workflow.Namespace != controller.config.Namespace || workflow.Annotations[runIDAnnotation] != runID {
		return nil, errors.New("argojob: synchronization observation identity mismatch")
	}
	observation := &model.SyncObservation{Holding: []string{}, Waiting: []string{}}
	if workflow.Status.Phase.Completed() || workflow.Status.Synchronization == nil {
		return observation, nil
	}
	holding, waiting := map[string]bool{}, map[string]bool{}
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
