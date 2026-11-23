package api_test

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/pkg/argoworkflow/argoworkflowtest"
)

func parallelExample(t *testing.T, base apicontract.RunRecord) apicontract.RunRecord {
	t.Helper()
	base.ID = strings.Repeat("8", 32)
	base.Steps = stateport.ProjectSteps(stateport.Run{Admission: admissioncontract.PreparedView{Plan: argoworkflowtest.Plan(t)}}, nil)
	for i := range base.Steps {
		s := &base.Steps[i]
		s.Status, s.StartedAt, s.FinishedAt = stepmodel.StepPassed, base.StartedAt, base.FinishedAt
		if s.Name == "install-systemd-tools" {
			s.Status = stepmodel.StepFailed
		}
		if s.Name == "scan-systemd-tools" {
			s.Status, s.StartedAt, s.FinishedAt = stepmodel.StepPending, nil, nil
		}
	}
	base.Failure = &stepmodel.FailureRecord{Step: "install-systemd-tools", Name: "install-systemd-tools"}
	base.DisplayReason = "step failed: exit 2"
	counts, err := stepmodel.CountSteps(base.Steps, true)
	if err != nil {
		t.Fatal(err)
	}
	base.LogicalStepCounts = counts
	return base
}

func TestParallelExampleUsesAdmittedTaskGraph(t *testing.T) {
	run := parallelExample(t, apicontract.RunRecord{})
	if len(run.Steps) != 44 || run.Steps[6].Name != "release-index" || run.Steps[8].Name != "deprecations" {
		t.Fatal("example is not the admitted task graph", run.Steps)
	}
}

func exampleRunGet(runs []apicontract.RunRecord, parallel apicontract.RunRecord, summaries apicontract.Handler[apicontract.RunGetArgs, apicontract.RunRecord]) apicontract.HandlerFunc[apicontract.RunGetArgs, apicontract.RunRecord] {
	return apicontract.HandlerFunc[apicontract.RunGetArgs, apicontract.RunRecord](func(ctx context.Context, caller apicontract.Caller, args apicontract.RunGetArgs) (apicontract.Call[apicontract.RunRecord], error) {
		if len(args.ID) == 32 && (args.ID[0] == 'a' || args.ID[0] == 'b') {
			return summaries.Prepare(ctx, caller, args)
		}
		return func(context.Context) (apicontract.RunRecord, error) {
			if strings.HasPrefix(parallel.ID, args.ID) {
				return parallel, nil
			}
			for _, run := range runs {
				if strings.HasPrefix(run.ID, args.ID) {
					return run, nil
				}
			}
			fallback := runs[1]
			fallback.ID = args.ID
			if args.ID == "77777777" {
				fallback = runs[0]
				fallback.ID = args.ID
				fallback.Ref = "feature/pass"
			}
			return fallback, nil
		}, nil
	})
}
