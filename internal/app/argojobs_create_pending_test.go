package app

import (
	"context"
	"errors"
	"testing"
)

type settlingArgoControl struct {
	configuredController
	settle func(context.Context, string, string) error
}

func (c *settlingArgoControl) SettleCreate(ctx context.Context, name, id string) error {
	return c.settle(ctx, name, id)
}

func TestArgoJobsPendingCreateSettlementBypassesInflightDeleteShortcut(t *testing.T) {
	for _, fail := range []bool{false, true} {
		jobs := &ArgoJobs{intents: map[string]argoIntent{"wf": {runID: "run", cancelled: true}}}
		called := false
		jobs.controller = &settlingArgoControl{settle: func(_ context.Context, name, id string) error {
			called = true
			if name != "wf" || id != "run" {
				t.Fatal("cleanup identity changed")
			}
			if fail {
				return context.DeadlineExceeded
			}
			return nil
		}}
		err := jobs.SettleCreate(t.Context(), "wf", "run")
		if !called || (err != nil) != fail {
			t.Fatalf("cleanup bypassed or error lost: %v", err)
		}
		if fail && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		if (len(jobs.intents) == 1) != fail {
			t.Fatal("cleanup forgot unresolved intent or retained confirmed one")
		}
	}
}
