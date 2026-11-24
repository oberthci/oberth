package app_test

import (
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

func TestReleaseBuildDoesNotSkipNewerRejectedAdmission(t *testing.T) {
	for _, file := range []string{"", workflowpolicy.BuildFile, ".oberth/walk.yaml"} {
		t.Run(file, func(t *testing.T) {
			r, history := releaseRequest(), buildHistory()
			rejected := history[0].Clone()
			rejected.ID, rejected.Sequence, rejected.Status = "rejected-newer", 2, stepmodel.RunFailed
			rejected.HasAdmission = false
			rejected.Admission = admissioncontract.PreparedView{}
			rejected.Origin.Trigger = model.TriggerBranch
			rejected.Origin.RequestedFile = file
			h := &releaseScanCounter{releaseHistory: append(releaseHistory{rejected}, history...)}
			facts, err := app.VerifiedReleaseFacts(t.Context(), r, workflowpolicy.ReleaseFile, h)
			if file == ".oberth/walk.yaml" {
				if err != nil || facts.BuildStatus != string(stepmodel.RunPassed) || h.reads != 1 {
					t.Fatal("optional rejection replaced build evidence", facts, err, h.reads)
				}
				return
			}
			expectRefusal(t, err, "release_build")
			if facts.BuildStatus != "" || h.reads != 0 {
				t.Fatal("rejected build reached older passing admission", facts, h.reads)
			}
		})
	}
}

func TestReleaseRejectedTagDoesNotBlockBuildEvidence(t *testing.T) {
	for _, file := range []string{"", workflowpolicy.BuildFile} {
		t.Run(file, func(t *testing.T) {
			r, history := releaseRequest(), buildHistory()
			rejected := history[0].Clone()
			rejected.ID, rejected.Sequence, rejected.Status = "rejected-tag", 2, stepmodel.RunFailed
			rejected.Ref, rejected.Release, rejected.HasAdmission = "refs/tags/v1.2.2", false, false
			rejected.Admission = admissioncontract.PreparedView{}
			rejected.Origin.Trigger, rejected.Origin.RequestedFile = model.TriggerTag, file
			h := &releaseScanCounter{releaseHistory: append(releaseHistory{rejected}, history...)}
			facts, err := app.VerifiedReleaseFacts(t.Context(), r, workflowpolicy.ReleaseFile, h)
			if err != nil || facts.BuildStatus != string(stepmodel.RunPassed) || h.reads != 1 {
				t.Fatal("rejected tag replaced passing build evidence", facts, err, h.reads)
			}
		})
	}
}
