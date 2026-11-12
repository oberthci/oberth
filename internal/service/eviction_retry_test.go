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

func evictionResult(name string, at time.Time) JobResult {
	return JobResult{Status: model.RunFailed, Phase: "infrastructure", Error: "Evicted: The node was low on resource: ephemeral-storage.",
		Steps:               []model.StepResult{{Stage: "ci", Step: "install-helm", Status: model.StepFailed, DeclaredSize: "M", StartedAt: &at, FinishedAt: &at}},
		TerminalObservation: &model.RunTerminalObservation{Version: 1, WorkflowName: name, Nodes: 2, Steps: []model.RunObservedStep{{Stage: "ci", Step: "install-helm", Status: model.StepFailed, Started: true, Evicted: true, StartedAt: &at, FinishedAt: &at}}},
	}
}

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
		"Evicted: Pod ephemeral local storage usage exceeds the total limit of containers 512Mi.": "evicted: ephemeral storage limit 512 MiB; raise the step's ephemeral-storage limit",
		"Evicted: The node was low on resource: memory.":                                          "evicted: node memory pressure",
		"Evicted: The node was low on resource: ephemeral-storage.":                               "evicted: node storage pressure",
		"Evicted": "evicted by kubelet",
	} {
		run := model.Run{Status: model.RunFailed, Phase: "infrastructure", Error: message}
		if got := presentationRun(run).DisplayReason; got != want {
			t.Errorf("%q != %q", got, want)
		}

	}
}

// Mirrors the controller's pre-create wrapper: false is not proof of a
// deterministic refusal, and the underlying error must remain inspectable.
type preparationRetryFailure struct{ cause error }

func (e *preparationRetryFailure) Error() string             { return e.cause.Error() }
func (e *preparationRetryFailure) Unwrap() error             { return e.cause }
func (*preparationRetryFailure) InfrastructureFailure() bool { return false }
func (*preparationRetryFailure) BeforeWorkflowCreate() bool  { return true }
