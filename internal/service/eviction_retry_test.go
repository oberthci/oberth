package service

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestDeterministicAdmissionRefusalsDoNotAutoRetry(t *testing.T) {
	for name, cause := range map[string]error{
		"configuration": &argoworkflow.AdmissionError{Cause: fmt.Errorf("invalid workflow configuration")},
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			f := newRecoveryFixtureWithOptions(t, store.Options{Now: func() time.Time { return now }})
			run := f.enqueueAndClaim(t, "feature/"+name, strings.Repeat("b", 40))
			jobs := &rcaCreateFailure{createErr: fmt.Errorf("admission: %w", cause)}
			if err := f.scheduler(t, jobs).executeAndCleanup(t.Context(), run); err != nil {
				t.Fatal(err)
			}
			failed, err := f.store.Run(t.Context(), run.ID)
			if err != nil || failed.Status != model.RunFailed {
				t.Fatalf("deterministic refusal retried: %+v %v", failed, err)
			}
		})
	}
}

func TestEvictionDisplayReason(t *testing.T) {
	for message, want := range map[string]string{
		"Evicted: Pod ephemeral local storage usage exceeds the total limit of containers 512Mi.":   "evicted: pod storage limit 512 MiB",
		"Evicted: The node was low on resource: memory.":                                            "evicted: node memory pressure",
		"Evicted: The node was low on resource: ephemeral-storage.":                                 "evicted: node storage pressure",
		"Evicted: Pod ephemeral local storage usage exceeds the total limit of containers unknown.": "evicted: pod storage limit",
		"Evicted: Container wait exceeded its local ephemeral storage limit":                        "evicted: container storage limit",
		"Evicted": "evicted by kubelet",
	} {
		run := model.Run{Status: model.RunFailed, Phase: "infrastructure", Error: message}
		if got := presentationRun(run).DisplayReason; got != want {
			t.Errorf("%q != %q", got, want)
		}
		for _, step := range []string{"", "test"} {
			run.FailedStep = step
			hint := runRetryHint(run, nil)
			if hint == nil || hint.Class != model.RetryInfrastructure || hint.Cause != "eviction" || hint.Action != "run_rerun" {
				t.Errorf("message %q step %q: infrastructure display contradicts retry hint %+v", message, step, hint)
			}
		}

	}
}
