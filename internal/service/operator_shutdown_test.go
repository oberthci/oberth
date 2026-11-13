package service

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestOperatorShutdownRetainsReasonWithoutInfrastructureHint(t *testing.T) {
	f := newAgentFixture(t)
	run := f.enqueue(t, "operator-stop", "a", false)
	reason := "Workflow stopped: Stopped with strategy 'Terminate'"
	ended, err := f.store.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunInterrupted, Phase: model.PhaseCancelled, Error: reason})
	if err != nil {
		t.Fatal(err)
	}
	if ended.Reason != reason {
		t.Fatalf("lost durable shutdown reason: %+v", ended)
	}
	shown := presentationRun(ended)
	if shown.DisplayStatus != "cancelled" || shown.DisplayReason != reason || runRetryHint(ended, nil) != nil {
		t.Fatalf("Workflow stop suggests infrastructure retry: %+v", shown)
	}
	f.recorder.handle(t.Context(), agentNotification{kind: "run", id: run.ID})
	page := f.events(t, agentActor, map[string]any{})
	if len(page.Events) != 1 {
		t.Fatalf("events: %+v", page.Events)
	}
	event := page.Events[0]
	if event.Data["retry_hint"] != nil || !strings.Contains(event.Summary, reason) {
		t.Fatalf("Workflow stop event: %+v", event)
	}
}
