package service

import (
	"fmt"

	"github.com/oberthci/oberth/internal/store"
)

// AcquireEvidencePurge excludes local scheduler writers, including a cancelled
// run whose durable record is terminal while its log stream is still closing.
// The caller holds the returned guard through the protected database purge.
func (scheduler *Scheduler) AcquireEvidencePurge(id string) (func(), error) {
	scheduler.evidenceMu.Lock()
	if scheduler.evidenceExecutions[id] > 0 {
		scheduler.evidenceMu.Unlock()
		return nil, fmt.Errorf("%w: run %s still has a scheduler execution", store.ErrEvidenceProtected, id)
	}
	return scheduler.evidenceMu.Unlock, nil
}

func (scheduler *Scheduler) beginEvidenceExecution(id string) func() {
	scheduler.evidenceMu.Lock()
	if scheduler.evidenceExecutions == nil {
		scheduler.evidenceExecutions = make(map[string]int)
	}
	scheduler.evidenceExecutions[id]++
	scheduler.evidenceMu.Unlock()
	return func() {
		scheduler.evidenceMu.Lock()
		defer scheduler.evidenceMu.Unlock()
		scheduler.evidenceExecutions[id]--
		if scheduler.evidenceExecutions[id] == 0 {
			delete(scheduler.evidenceExecutions, id)
		}
	}
}
