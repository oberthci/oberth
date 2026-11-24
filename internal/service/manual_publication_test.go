package service_test

import (
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

func TestSelectedWorkflowNeverPublishesBranch(t *testing.T) {
	for _, tc := range []struct {
		name, requested, selected, rerun string
		publications                     int
	}{
		{"ordinary build", "", workflowpolicy.BuildFile, "", 1},
		{"selected build", workflowpolicy.BuildFile, workflowpolicy.BuildFile, "", 0},
		{"selected optional", ".oberth/walk.yaml", ".oberth/walk.yaml", "", 0},
		{"selected rerun", ".oberth/walk.yaml", ".oberth/walk.yaml", "original", 0},
		{"sealed optional", "", ".oberth/walk.yaml", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			r := f.seed(stepmodel.RunRunning)
			r.Origin.RequestedFile, r.Admission.SelectedFile = tc.requested, tc.selected
			r.Origin.RerunOf = tc.rerun
			f.store.native.SeedRun(r)
			if err := f.s.Finished(t.Context(), service.FinishedRun{RunID: r.ID, Status: stepmodel.RunPassed}); err != nil {
				t.Fatal(err)
			}
			stored, err := f.store.Run(t.Context(), r.ID)
			if err != nil || stored.Status != stepmodel.RunPassed {
				t.Fatal("publication rule changed the run result", stored, err)
			}
			if got := len(f.store.native.PublicationValues()); got != tc.publications {
				t.Fatal("selected workflow published its branch or ordinary build lost publication", got)
			}
		})
	}
}

func TestSelectedWorkflowRuleKeepsReleasePublication(t *testing.T) {
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	r.Ref, r.Origin.Trigger, r.Release = "refs/tags/v1.2.3", model.TriggerTag, true
	r.Admission.SelectedFile = workflowpolicy.ReleaseFile
	f.store.native.SeedRun(r)
	if err := f.s.Finished(t.Context(), service.FinishedRun{RunID: r.ID, Status: stepmodel.RunPassed}); err != nil {
		t.Fatal(err)
	}
	publications := f.store.native.PublicationValues()
	if len(publications) != 1 {
		t.Fatal("normal release lost its publication", publications)
	}
	p := publications[0]
	if p.Ref != r.Ref || p.PreviousSHA == nil || *p.PreviousSHA != "" {
		t.Fatal("normal release lost tag publication fencing", p)
	}
}
