package service_test

import (
	"encoding/json"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestRunAbsenceNamesRunsWithoutRevealingHiddenRuns(t *testing.T) {
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	r.Repo = "private/team/widget"
	f.store.native.SeedRun(r)
	var first string
	for _, id := range []string{r.ID, "deadbeef000000000000000000000000"} {
		_, getErr := f.s.PrepareRunGet(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.RunGetArgs{ID: id})
		_, logErr := f.s.PrepareRunLogs(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.RunLogsArgs{ID: id})
		for _, err := range []error{getErr, logErr} {
			p := publicerr.Classify(err)
			if p.Code() != publicerr.NotFound || p.Reason() != "Run not found." || p.Remedy() != "List runs with oberth run list and select a visible run." {
				t.Fatal("unhelpful run absence", err)
			}
			raw, marshalErr := json.Marshal(p)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			if first == "" {
				first = string(raw)
			}
			if string(raw) != first {
				t.Fatal("hidden and absent run refusals differ", string(raw), first)
			}
		}
	}
}
