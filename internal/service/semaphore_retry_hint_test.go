package service

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestSemaphoreConfigurationRetryHint(t *testing.T) {
	for _, message := range []string{
		`Failed to acquire the synchronization lock. failed to initialize semaphore pipelines/ConfigMap/oberth-sync-limits/build with limit`,
		`failed to initialize semaphore pipelines/ConfigMap/oberth-sync-limits/build with limit`,
		`Failed to acquire the synchronization lock. configmaps "oberth-sync-limits" not found`,
		`Failed to acquire the synchronization lock. Sync configuration key 'build' not found in ConfigMap`,
	} {
		for _, phase := range []string{"job", "infrastructure", "test"} {
			run := model.Run{Status: model.RunFailed, Phase: phase, Error: message}
			before := run
			hint := runRetryHint(run, nil)
			if hint == nil || hint.Class != model.RetryCode || hint.Cause != "synchronization_configuration" || strings.Contains(hint.Action, "run_retry") {
				t.Fatalf("hint = %+v for %s", hint, message)
			}
			for _, word := range []string{"operator", "ConfigMap", "key", "namespace", "Workflow YAML"} {
				if !strings.Contains(hint.Action, word) {
					t.Fatalf("missing %s: %+v", word, hint)
				}
			}
			if run.Status != before.Status || run.Phase != before.Phase || run.Error != before.Error {
				t.Fatal("hint changed run")
			}
		}
	}
	for _, message := range []string{
		"connection refused", "Failed to acquire the synchronization lock. connection refused",
		"Failed to acquire the synchronization lock. failed to initialize semaphore pipelines/Semaphore/oberth-sync-limits/build with limit",
		"failed to initialize semaphore pipelines/ConfigMap/oberth-sync-limits/bad/key with limit",
		"failed to initialize semaphore pipelines/ConfigMap/oberth-sync-limits/build with limit trailing text",
		`configmaps "unrelated" not found`,
		"failed to initialize semaphore pipelines/ConfigMap/other/build with limit",
		"prefix failed to initialize semaphore pipelines/ConfigMap/oberth-sync-limits/build with limit",
	} {
		hint := runRetryHint(model.Run{Status: model.RunFailed, Phase: "infrastructure", Error: message}, nil)
		if hint == nil || hint.Class != model.RetryInfrastructure {
			t.Fatalf("unrelated error reclassified: %+v", hint)
		}
	}
	if hint := runRetryHint(model.Run{Status: model.RunRunning, Error: "failed to initialize semaphore pipelines/ConfigMap/oberth-sync-limits/build with limit"}, nil); hint != nil {
		t.Fatal("active run has retry hint", hint)
	}
}
