package service_test

import (
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stepmodel"
	"testing"
)

func TestOptionalWorkflowCannotSupplyPromotionEvidence(t *testing.T) {
	for _, build := range []stepmodel.RunStatus{stepmodel.RunFailed, stepmodel.RunPassed} {
		t.Run(string(build), func(t *testing.T) {
			f := setup(t)
			r := f.seed(build)
			optional := r
			// Let the store assign a sequence strictly after the build.
			optional.ID, optional.Sequence, optional.Status = "abcdef01000000000000000000000099", 0, stepmodel.RunPassed
			optional.Admission.SelectedFile = ".oberth/walk.yaml"
			f.store.native.SeedRun(optional)
			call, err := f.s.PreparePromote(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.PromoteArgs{Repo: repository, SHA: head, Reason: "land tested work"})
			if build == stepmodel.RunFailed {
				if err == nil {
					t.Fatal("optional pass replaced failing build evidence")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			result, err := call(t.Context())
			if err != nil || result.TestedRunID != r.ID {
				t.Fatal("optional workflow replaced passing build", result, err)
			}
		})
	}
}

func TestRejectedOptionalWorkflowCannotReplaceBuildEvidence(t *testing.T) {
	f := setup(t)
	r := f.seed(stepmodel.RunPassed)
	optional := r
	optional.ID, optional.Sequence, optional.Status = "abcdef01000000000000000000000099", 0, stepmodel.RunFailed
	optional.HasAdmission, optional.Admission = false, admissioncontract.PreparedView{}
	optional.Origin.RequestedFile = ".oberth/walk.yaml"
	f.store.native.SeedRun(optional)
	call, err := f.s.PreparePromote(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.PromoteArgs{Repo: repository, SHA: head, Reason: "land tested work"})
	if err != nil {
		t.Fatal("rejected optional workflow masked passing build", err)
	}
	result, err := call(t.Context())
	if err != nil || result.TestedRunID != r.ID {
		t.Fatal("rejected optional workflow replaced passing build", result, err)
	}
}

func TestRejectedOptionalWorkflowCannotCreateBuildIssue(t *testing.T) {
	f := setup(t)
	optional := ciRun("abcdef01000000000000000000000032", "refs/heads/main", head, model.TriggerBranch)
	optional.HasAdmission, optional.Admission = false, admissioncontract.PreparedView{}
	optional.Origin.RequestedFile = ".oberth/walk.yaml"
	f.store.native.SeedRun(optional)
	finishCI(t, f, optional.ID, stepmodel.RunFailed)
	if issues := ciIssues(t, f); len(issues) != 0 {
		t.Fatal("rejected optional workflow created a build issue", issues)
	}
}

func TestOptionalWorkflowCannotCloseBuildIssue(t *testing.T) {
	f := setup(t)
	red := seedCI(f, "abcdef01000000000000000000000031", "refs/heads/main", model.TriggerBranch)
	finishCI(t, f, red.ID, stepmodel.RunFailed)
	optional := ciRun("abcdef01000000000000000000000032", red.Ref, head, model.TriggerBranch)
	optional.Admission.SelectedFile = ".oberth/walk.yaml"
	f.store.native.SeedRun(optional)
	finishCI(t, f, optional.ID, stepmodel.RunPassed)
	issues := ciIssues(t, f)
	if len(issues) != 1 || issues[0].State != model.IssueOpen || issues[0].CIWorkID != red.ID {
		t.Fatal("optional pass cleared build failure", issues)
	}
}
