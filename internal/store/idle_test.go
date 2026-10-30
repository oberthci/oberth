package store

import (
	"context"
	"errors"
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

func TestIdleQueueReadersStayFreshDuringWriterHold(t *testing.T) {
	now := time.Now()
	database := testStore(t, &now)
	repo := createRepo(t, database)
	if err := database.OpenIdleQueueReaders(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := database.OpenIdleQueueReaders(t.Context()); err != nil {
		t.Fatal(err)
	}
	queued, err := database.EnqueueRun(t.Context(), testRunSpec(repo.ID, "feature/idle-reader", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(t.Context(), `UPDATE runs SET trigger='apply' WHERE id=?`, queued.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := database.db.Conn(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("primary connection was not held: %v", err)
	}
	ctxRead, stopRead := context.WithTimeout(t.Context(), time.Second)
	defer stopRead()
	state, err := database.IdleQueueStatus(ctxRead)
	if err != nil || state.Queued != 1 || state.Running != 0 || state.Branch != 1 {
		t.Fatalf("queue read blocked or saw uncommitted state: %+v %v", state, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	run, err := database.ClaimNextRun(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunPassed}); err != nil {
		t.Fatal(err)
	}
	state, err = database.IdleQueueStatus(t.Context())
	if err != nil || state.Queued+state.Running != 0 || !state.Valid() {
		t.Fatalf("queue reader retained an old snapshot after commit: %+v %v", state, err)
	}
}

func TestIdleQueueReadersAreReadOnlyBoundedAndClosed(t *testing.T) {
	now := time.Now()
	database := testStore(t, &now)
	if err := database.OpenSSHAuthReaders(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := database.OpenIdleQueueReaders(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := database.idleDB.ExecContext(t.Context(), `DELETE FROM runs`); err == nil {
		t.Fatal("queue reader accepted a write")
	}
	if database.idleDB.Stats().MaxOpenConnections != 1 {
		t.Fatal("queue reader connection count is unbounded")
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := database.IdleQueueStatus(t.Context()); err == nil {
		t.Fatal("queue reader remained open after store close")
	}
	if err := database.authDB.PingContext(t.Context()); err == nil {
		t.Fatal("SSH auth readers remained open after store close")
	}
}
