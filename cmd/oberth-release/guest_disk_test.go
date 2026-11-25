package main

import (
	"os"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestGuestDiskInspectionAdmitsWithoutCapability(t *testing.T) {
	body, err := os.ReadFile("../../.oberth/guest-disk.yaml")
	if err != nil {
		t.Fatal(err)
	}
	r := ownRequest(body, "guest-disk", model.TriggerBranch)
	p, err := argoworkflow.Prepare(t.Context(), r, ownPolicy(), ownHooks(t, r))
	if err != nil {
		t.Fatal("disk inspection requires more than run-owned storage", err)
	}
	if len(p.View().Plan.Entries) != 1 || !p.View().Plan.Exact {
		t.Fatal("disk inspection plan differs", p.View().Plan)
	}
	w, err := argoworkflow.Decode(body)
	if err != nil {
		t.Fatal(err)
	}
	if w.Spec.VolumeClaimGC == nil || w.Spec.VolumeClaimGC.Strategy != wfv1.VolumeClaimGCOnCompletion {
		t.Fatal("disk inspection retains its scratch claim")
	}
}
