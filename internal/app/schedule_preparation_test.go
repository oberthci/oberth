package app_test

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestSchedulePreparationRetainsEmptyIdentityAndRejectsDeferredChecks(t *testing.T) {
	t.Parallel()
	for _, live := range []bool{true, false} {
		f := newFixture(t)
		if !live {
			f.native.Hook.VerifyIdentities = func(_ context.Context, q admissioncontract.IdentityRequest) (admissioncontract.IdentityEvidence, error) {
				return admissioncontract.IdentityEvidence{Namespace: q.Namespace, Pipeline: admissioncontract.AccountEvidence{Name: q.PipelineAccount}, State: admissioncontract.StateLiveRequired}, nil
			}
		}
		s := f.new(t)
		r := request()
		r.ScheduleEntry = "daily"
		prepared, err := s.PrepareSchedule(t.Context(), r)
		if !live {
			if err == nil || prepared != nil {
				t.Fatal("deferred native verification produced an executable parent")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		assertScheduleParent(t, prepared)

		rows, err := f.runs.Runs(t.Context(), stateport.RunFilter{Limit: 10})
		if err != nil || len(rows) != 0 || f.creates.Load() != 0 {
			t.Fatal("preparing a parent started a run", err)
		}
	}
}

func assertScheduleParent(t *testing.T, prepared *admissioncontract.PreparedAdmission) {
	t.Helper()
	v := prepared.View()
	if v.RunID != "" || v.Trigger != model.TriggerSchedule || v.ScheduleEntry != "daily" {
		t.Fatal("schedule preparation became a run", v.RunID, v.Trigger, v.ScheduleEntry)
	}
}
