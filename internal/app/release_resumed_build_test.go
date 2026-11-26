package app_test

import (
	"testing"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

// A newer resumed run of the tested commit neither blocks nor supplies the
// release's build evidence; the same run without the resumed mark blocks it.
func TestReleaseBuildSkipsResumedRun(t *testing.T) {
	for _, status := range []stepmodel.RunStatus{stepmodel.RunPassed, stepmodel.RunFailed} {
		t.Run(string(status), func(t *testing.T) {
			r, history := releaseRequest(), buildHistory()
			resumed := history[0].Clone()
			resumed.ID, resumed.Sequence, resumed.Status = "resumed-newer", 2, status
			resumed.Origin.Assisted, resumed.Origin.ResumedFrom = true, history[0].ID
			h := &releaseScanCounter{releaseHistory: append(releaseHistory{resumed}, history...)}
			facts, err := app.VerifiedReleaseFacts(t.Context(), r, workflowpolicy.ReleaseFile, h)
			if err != nil || facts.BuildStatus != string(stepmodel.RunPassed) {
				t.Fatal("resumed run decided the release build", facts, err)
			}
			resumed.Origin.ResumedFrom = ""
			h = &releaseScanCounter{releaseHistory: append(releaseHistory{resumed}, history...)}
			_, err = app.VerifiedReleaseFacts(t.Context(), r, workflowpolicy.ReleaseFile, h)
			expectRefusal(t, err, "release_build")
		})
	}
}
