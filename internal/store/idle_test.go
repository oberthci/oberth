package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestIdleQueueStatusIncludesEveryActiveTrigger(t *testing.T) {
	now := time.Now().UTC()
	database := testStore(t, &now)
	repo := createRepo(t, database)
	triggers := []string{"branch", "schedule", "promotion", "plan", "apply", "release", "unknown"}
	for i, trigger := range triggers {
		spec := testRunSpec(repo.ID, fmt.Sprintf("feature/idle-%d", i), strings.Repeat("a", 40))
		spec.Trigger = trigger
		if trigger == "release" {
			spec.RefKind = model.RefTag
			spec.Release = true
		}
		_, err := database.EnqueueRun(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			run, err := database.ClaimNextRun(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.SetRunJobName(t.Context(), run.ID, "fixture-workflow-"+run.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := database.MarkRunSubmitted(t.Context(), run.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	state, err := database.IdleQueueStatus(t.Context())
	if err != nil || !state.Valid() || state.Queued+state.Running != 7 || state.Branch != 2 || state.Critical() != 5 || state.Tag != 1 || state.Promotion != 1 || state.Plan != 1 || state.Apply != 1 || state.Other != 1 {
		t.Fatalf("%+v %v", state, err)
	}
	// Completed history contributes no live work.
	for {
		run, err := database.ClaimNextRun(t.Context())
		if err != nil {
			break
		}
		if _, err := database.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunPassed}); err != nil {
			t.Fatal(err)
		}
	}
	after, err := database.IdleQueueStatus(t.Context())
	if err != nil || !after.Valid() || after.Queued != 0 {
		t.Fatalf("%+v %v", after, err)
	}
}
