package fake

import (
	"fmt"
	"k8s.io/utils/clock"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestLatestRunClonesOnlyReturnedHistory(t *testing.T) {
	f := NewRuns((clock.RealClock{}).Now)
	for i := range 2000 {
		id := fmt.Sprint(i)
		f.data.runs[id] = stateport.Run{RunSummary: stepmodel.RunSummary{ID: id, Repo: "origin/team/repo", Ref: "refs/heads/main"},
			Sequence: int64(i + 1), Findings: []admissioncontract.Finding{{}}}
	}
	filter := stateport.RunFilter{LatestRepository: true, Limit: 50}
	allocs := testing.AllocsPerRun(3, func() {
		rows, err := f.Runs(t.Context(), filter)
		if err != nil || len(rows) != 1 || rows[0].Sequence != 2000 {
			t.Fatal(rows, err)
		}
	})
	t.Logf("latest of 2000 runs: %.0f allocations", allocs)
	if allocs > 100 {
		t.Fatal("latest query clones discarded histories", allocs)
	}
}
