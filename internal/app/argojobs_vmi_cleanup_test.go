package app

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type failingVMICleanupController struct{ *blockingController }

func (failingVMICleanupController) CleanupVMIs(_ context.Context, _, _ string) error {
	return errors.New("virt-api unavailable")
}

func TestVMICleanupFailureDoesNotChangeCompletedRun(t *testing.T) {
	var audited model.AuditActionSpec
	jobs := &ArgoJobs{
		controller: failingVMICleanupController{newBlockingController()},
		auditor:    &stubAuditor{onAppend: func(spec model.AuditActionSpec) { audited = spec }},
		intents:    map[string]argoIntent{"run-workflow": {runID: "run-123", trigger: periapsis.TriggerCI, created: true}},
	}
	result, err := jobs.Wait(t.Context(), "run-workflow", io.Discard)
	if err != nil || result.Status != model.RunPassed {
		t.Fatalf("terminal result lost to VMI cleanup: %+v, %v", result, err)
	}
	if audited.Action != "ci.argo.vmi.cleanup-failed" || audited.ResourceID != "run-123" || !strings.Contains(audited.Details, "virt-api unavailable") {
		t.Fatalf("cleanup failure was not audited: %+v", audited)
	}
}
