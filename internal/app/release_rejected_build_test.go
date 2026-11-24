package app_test

import (
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/app"
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
