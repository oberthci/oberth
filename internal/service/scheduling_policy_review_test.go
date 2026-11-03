package service

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func TestSchedulingQueuedIgnoresStalePolicyWait(t *testing.T) {
	run := model.Run{ID: "recovered", Status: model.RunQueued, Reason: "retried after restart; " + store.PolicyRefreshWaitingReason}
	got, _ := (&API{}).observeScheduling(t.Context(), run, nil, false)
	if got.State != "queued" {
		t.Fatalf("recovered queue retains obsolete policy wait: %#v", got)
	}
}

func TestPolicyWaitKeepsSharedDisplayStatusAndReason(t *testing.T) {
	started := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		phase     string
		startedAt *time.Time
		status    string
	}{
		{"before admission", "waiting", nil, model.DisplayQueued},
		{"publication", "publishing", &started, model.DisplayRunning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := model.Run{ID: "policy-wait", Status: model.RunRunning, Phase: tc.phase, StartedAt: tc.startedAt, Reason: store.PolicyRefreshWaitingReason}
			observation, reason := (&API{}).observeScheduling(t.Context(), run, nil, false)
			view := presentationRun(run)
			applyQueuedDisplay(&view, observation, reason)
			if view.DisplayStatus != tc.status || view.DisplayReason != store.PolicyRefreshWaitingReason {
				t.Fatalf("policy wait lost shared status or cause: %#v", view)
			}
			if observation.State != "waiting" || observation.Message != store.PolicyRefreshWaitingReason+": waiting for upstream trusted test policy; no publication is allowed." {
				t.Fatalf("policy wait lost its machine observation: %#v", observation)
			}
		})
	}
}
