package service

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func TestRunListWithdrawsSuspensionImmediatelyWhenWatchUnhealthy(t *testing.T) {
	f := newControlFixture(t)
	run := submittedDisplayRun(t, f, "watch-outage")
	s := f.api(t)
	healthy := true
	const unavailable = "status unknown: cluster API unreachable since 12:34 UTC"
	s.schedulingObserver = schedulingObserverFunc(func(context.Context, model.Run) (*model.ExecutionObservation, error) {
		if !healthy {
			return &model.ExecutionObservation{State: "unavailable", Message: unavailable}, nil
		}
		return &model.ExecutionObservation{State: "observed", Phase: "Running", Reason: "Suspended", Message: model.WorkflowSuspendedMessage}, nil
	})
	for _, want := range []string{model.WorkflowSuspendedMessage, unavailable, model.WorkflowSuspendedMessage} {
		value, err := s.Runs(t.Context(), api.Actor{Identity: "owner"}, api.RunFilter{})
		if err != nil {
			t.Fatal(err)
		}
		rows := value.([]model.Run)
		if len(rows) != 1 || rows[0].DisplayReason != want {
			t.Fatal(rows)
		}
		durable, err := f.store.Run(t.Context(), run.ID)
		if err != nil || durable.Status != model.RunRunning || durable.Error != "" {
			t.Fatal(durable, err)
		}
		healthy = !healthy
	}
}
