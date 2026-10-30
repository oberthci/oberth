package app

import (
	"context"
	"testing"
	"time"
)

type settlingArgoControl struct {
	configuredController
	settle func(context.Context, string, string, time.Time) (bool, error)
}

func (c *settlingArgoControl) SettleCreate(ctx context.Context, name, id string, boundary time.Time) (bool, error) {
	return c.settle(ctx, name, id, boundary)
}
func TestArgoJobsPendingCreateSettlementBypassesInflightDeleteShortcut(t *testing.T) {
	for _, mode := range []string{"done", "pending", "error"} {
		jobs := &ArgoJobs{intents: map[string]argoIntent{"wf": {runID: "run", cancelled: true}}}
		boundary := time.Now().Add(time.Minute)
		called := false
		jobs.controller = &settlingArgoControl{settle: func(_ context.Context, name, id string, notBefore time.Time) (bool, error) {
			called = true
			if name != "wf" || id != "run" || notBefore != boundary {
				t.Fatal("cleanup identity/boundary changed")
			}
			if mode == "error" {
				return false, context.DeadlineExceeded
			}
			return mode == "done", nil
		}}
		done, err := jobs.SettleCreate(t.Context(), "wf", "run", boundary)
		if !called || done != (mode == "done") || (err != nil) != (mode == "error") {
			t.Fatalf("cleanup shortcut/error: %t %v", done, err)
		}
		if (len(jobs.intents) == 1) != (mode != "done") {
			t.Fatal("unconfirmed intent forgotten")
		}
	}
}
