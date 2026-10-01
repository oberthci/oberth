package auditanchor

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// T1: TestManagerWitnessOnlyModeAcceptsAdoptionIntentOverNonGenesisChain
func TestManagerWitnessOnlyModeAcceptsAdoptionIntentOverNonGenesisChain(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	database, err := store.OpenAdminClient(ctx, filepath.Join(t.TempDir(), "oberth.sqlite"),
		store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()

	// Seed a non-genesis chain (>= 2 actions).
	var lastAction model.AuditAction
	for _, id := range []string{"one", "two"} {
		now = now.Add(time.Second)
		lastAction, err = database.AppendAuditAction(ctx, model.AuditActionSpec{
			Actor: "agent@host", Action: "issue.create", ResourceType: "issue", ResourceID: id,
			Details: `{"title":"test"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// Seed continuity with a seq-1 intent binding the exact head (adoption state).
	continuity := newTestContinuity(t)
	if err := continuity.Prepare(ctx, model.AuditWitnessIntent{
		Sequence: 1, AuditID: lastAction.ID,
		AuditSHA256: append([]byte(nil), lastAction.SHA256...),
	}); err != nil {
		t.Fatal(err)
	}

	// Witness-only (authority == nil): the adoption intent should make VerifyStartup pass.
	external := &fakeWitness{now: &now}
	manager, err := NewManager(ManagerConfig{
		Store: database, Witness: external, Continuity: continuity,
		Interval: 10 * time.Minute, MaxAge: 30 * time.Minute,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.VerifyStartup(ctx); err != nil {
		t.Fatalf("VerifyStartup with adoption intent: %v", err)
	}

	// Initialize publishes witness 1 binding the head.
	if err := manager.Initialize(ctx); err != nil {
		t.Fatalf("Initialize after adoption VerifyStartup: %v", err)
	}
	if len(external.history) != 1 || external.history[0].AuditID != lastAction.ID {
		t.Fatalf("witness history = %#v, want one entry binding action %d", external.history, lastAction.ID)
	}

	// AllowMutation passes.
	if err := manager.AllowMutation(ctx); err != nil {
		t.Fatalf("AllowMutation after adoption: %v", err)
	}

	// A fresh manager restart with the same continuity/witness also passes.
	restarted, err := NewManager(ManagerConfig{
		Store: database, Witness: external, Continuity: continuity,
		Interval: 10 * time.Minute, MaxAge: 30 * time.Minute,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.VerifyStartup(ctx); err != nil {
		t.Fatalf("VerifyStartup after restart: %v", err)
	}
}
