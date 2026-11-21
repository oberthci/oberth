package app_test

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestSchedulePreparationRequiresLiveEvidenceWithoutStartingRun(t *testing.T) {
	t.Parallel()
	for _, live := range []bool{true, false} {
		t.Run(map[bool]string{true: "verified", false: "deferred"}[live], func(t *testing.T) {
			f := newFixture(t)
			if !live {
				f.native.Hook.VerifyIdentities = func(_ context.Context, r admissioncontract.IdentityRequest) (admissioncontract.IdentityEvidence, error) {
					return admissioncontract.IdentityEvidence{Namespace: r.Namespace, State: admissioncontract.StateLiveRequired}, nil
				}
			}
			r := request()
			r.ScheduleEntry = "nightly"
			p, err := f.new(t).PrepareSchedule(t.Context(), r)
			assertSchedulePreparation(t, p, err, live)
			rows, err := f.runs.Runs(t.Context(), stateport.RunFilter{Limit: 10})
			if err != nil || len(rows) != 0 || f.creates.Load() != 0 {
				t.Fatal("schedule preparation started a run", rows, err)
			}
		})
	}
}

func assertSchedulePreparation(t *testing.T, p *admissioncontract.PreparedAdmission, err error, live bool) {
	t.Helper()
	if !live {
		if err == nil || p != nil {
			t.Fatal("deferred evidence authorized a schedule")
		}
		return
	}
	if err != nil || p.View().RunID != "" || p.View().Trigger != model.TriggerSchedule || p.View().ScheduleEntry != "nightly" {
		t.Fatal("schedule preparation lost its reusable identity", p, err)
	}
}
