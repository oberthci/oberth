package service

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestInfrastructureRetryKeepsPartialFailedStepAttribution(t *testing.T) {
	f := newRecoveryFixture(t)
	run := f.enqueueAndClaim(t, "feature/partial-failure", strings.Repeat("e", 40))
	jobs := &recoveryJobs{waitErr: &identityObservationFailure{cause: context.DeadlineExceeded}, terminalErr: ErrJobNotTerminal, waitResult: JobResult{Status: model.RunFailed, Phase: "test", FailedStage: "ci", FailedStep: "unit"}}
	if err := f.scheduler(t, jobs).executeAndCleanup(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	failed, err := f.store.Run(t.Context(), run.ID)
	if err != nil || failed.FailedStep != "unit" {
		t.Fatalf("observation error erased test failure: %+v %v", failed, err)
	}
}
