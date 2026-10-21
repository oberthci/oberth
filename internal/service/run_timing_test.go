package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func TestRunGetRetainsVersionedTimingWithoutCluster(t *testing.T) {
	fixture := newControlFixture(t, JobResult{Status: model.RunPassed})
	ctx := context.Background()
	run, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{EventID: "run-timing", Repository: fixture.repo, Branch: "feature/timing", SHA: strings.Repeat("a", 40), Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.scheduler.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	expected, err := fixture.store.RunTiming(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if expected.ClaimedAt == nil || expected.CheckoutReadyAt == nil || expected.TerminalAt == nil || len(expected.Admissions) != 1 || expected.Admissions[0].RequestedAt == nil || expected.Admissions[0].GrantedAt == nil {
		t.Fatalf("missing lifecycle evidence: %+v", expected)
	}
	value, err := fixture.api(t).CallTool(ctx, api.Actor{Identity: "agent@host"}, "run_get", json.RawMessage(`{"id":"`+run.ID[:12]+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	detail := value.(RunDetailResponse)
	if detail.Timing == nil {
		t.Fatal("native run_get lost timing reader")
	}
	a, _ := json.Marshal(expected)
	b, _ := json.Marshal(detail.Timing)
	if string(a) != string(b) {
		t.Fatalf("run_get lost durable evidence: %s / %s", a, b)
	}
	if detail.Timing.WorkflowCreatedAt != nil || detail.Timing.Workflow != nil {
		t.Fatal("fake job submission inferred a Workflow observation")
	}
}
