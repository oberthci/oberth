package service_test

import (
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestRunViewsShowExplicitFileIncludingRefusedAdmission(t *testing.T) {
	for _, admitted := range []bool{false, true} {
		f := setup(t)
		r := f.seed(stepmodel.RunPassed)
		r.Origin.RequestedFile = "hack/walk-vm.yaml"
		r.HasAdmission = admitted
		r.Admission.SelectedFile = r.Origin.RequestedFile
		if !admitted {
			r.Status = stepmodel.RunFailed
			r.Admission = admissioncontract.PreparedView{}
		}
		f.store.native.SeedRun(r)
		got, err := f.s.RunGet(t.Context(), caller(), viewQuery{ID: r.ID})
		if err != nil || got.SelectedFile != r.Origin.RequestedFile {
			t.Fatal("run detail lost the explicit workflow", got, err)
		}
		page, err := f.s.RunList(t.Context(), caller(), viewQuery{Repo: repository})
		if err != nil || len(page.Runs) != 1 || page.Runs[0].SelectedFile != r.Origin.RequestedFile {
			t.Fatal("run list lost the explicit workflow", page, err)
		}
	}
}
