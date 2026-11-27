package argojob

import (
	"testing"

	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestNativeRawMessageChurnIsNotPublicEvidence(t *testing.T) {
	t.Parallel()
	before := []stateport.Node{{NodeID: "own", PodUID: "pod", Status: stepmodel.StepQueued, Message: "Insufficient cpu: first"}}
	after := []stateport.Node{{NodeID: "own", PodUID: "pod", Status: stepmodel.StepQueued, Message: "Insufficient cpu: second"}}
	if !sameNodeFacts(before, after) {
		t.Fatal("raw text churn would allocate an evidence generation")
	}
	after[0].PodUID = "replacement"
	if sameNodeFacts(before, after) {
		t.Fatal("Pod replacement was treated as raw text churn")
	}
	after[0].PodUID, after[0].Status = "pod", stepmodel.StepRunning
	if sameNodeFacts(before, after) {
		t.Fatal("native status change was discarded")
	}
}
