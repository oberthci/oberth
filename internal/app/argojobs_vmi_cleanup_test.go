package app

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type failingVMICleanupController struct {
	*blockingController
	onFailure func(context.Context, string, error)
}

func (controller *failingVMICleanupController) WithVMICleanupFailure(observe func(context.Context, string, error)) {
	controller.onFailure = observe
}

func (controller *failingVMICleanupController) Wait(ctx context.Context, name, runID string, destination io.Writer) (argojob.Completion, error) {
	controller.onFailure(ctx, runID, errors.New("virt-api unavailable"))
	return controller.blockingController.Wait(ctx, name, runID, destination)
}

func TestVMICleanupFailureDoesNotChangeCompletedRun(t *testing.T) {
	var audited model.AuditActionSpec
	controller := &failingVMICleanupController{blockingController: newBlockingController()}
	jobs := &ArgoJobs{
		controller: controller,
		auditor:    &stubAuditor{onAppend: func(spec model.AuditActionSpec) { audited = spec }},
		intents:    map[string]argoIntent{"run-workflow": {runID: "run-123", trigger: periapsis.TriggerCI, created: true}},
	}
	controller.WithVMICleanupFailure(jobs.auditVMICleanupFailure)
	result, err := jobs.Wait(t.Context(), "run-workflow", io.Discard)
	if err != nil || result.Status != model.RunPassed {
		t.Fatalf("terminal result lost to VMI cleanup: %+v, %v", result, err)
	}
	if audited.Action != "ci.argo.vmi.cleanup-failed" || audited.ResourceID != "run-123" || !strings.Contains(audited.Details, "virt-api unavailable") {
		t.Fatalf("cleanup failure was not audited: %+v", audited)
	}
}
