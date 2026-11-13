package service

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestSemaphoreErrorsRemainNative(t *testing.T) {
	for _, message := range []string{
		`Failed to acquire the synchronization lock. failed to initialize semaphore pipelines/ConfigMap/oberth-sync-limits/build with limit`,
		`failed to initialize semaphore pipelines/ConfigMap/oberth-sync-limits/build with limit`,
		`Failed to acquire the synchronization lock. configmaps "oberth-sync-limits" not found`,
		`Failed to acquire the synchronization lock. Sync configuration key 'build' not found in ConfigMap`,
		`Failed to acquire the synchronization lock. controller detail in another format`,
	} {
		for _, phase := range []string{"job", "infrastructure", "test"} {
			run := model.Run{Status: model.RunFailed, Phase: phase, Error: message}
			hint := runRetryHint(run, nil)
			if hint == nil || hint.Cause == "synchronization_configuration" {
				t.Fatalf("native error received a parsed semaphore hint: %+v", hint)
			}
			if !strings.Contains(failedRunDisplayReason(run), message) || run.Error != message {
				t.Fatalf("Argo message changed: %+v", run)
			}
		}
	}
}
