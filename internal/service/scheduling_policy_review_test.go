package service

import (
	"testing"

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
