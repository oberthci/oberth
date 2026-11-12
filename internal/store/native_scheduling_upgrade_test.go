package store

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestNativeSchedulingUpgradeMaterializesReceivesAndPreservesReplay(t *testing.T) {
	s := newMigrationIDTestStore(t)
	ctx := t.Context()
	cut := -1
	for i, m := range migrations {
		if strings.Contains(m.id, "remove-run-fair-share") {
			cut = i
			break
		}
	}
	if cut < 0 {
		t.Fatal("missing native scheduling upgrade")
	}
	if err := s.migrateWithMigrations(ctx, nil, migrations[:cut]); err != nil {
		t.Fatal(err)
	}
	if err := s.configureLiveReaders(ctx, 0); err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	running, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "running", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	queued, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "queued", strings.Repeat("b", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE runs SET phase='infrastructure-pending',error='Resource admission expired: no scheduled pod or progress; cleanup pending' WHERE id=?`, running.ID); err != nil {
		t.Fatal(err)
	}
	saved := map[string]model.RunSpec{}
	events := map[string]model.ReceiveEventSpec{}
	for _, id := range []string{"active", "deleted"} {
		spec := testRunSpec(repo.ID, id, strings.Repeat("c", 40))
		saved[id] = spec
		event := model.ReceiveEventSpec{ID: id, Actor: spec.Actor, RepoID: repo.ID, RefKind: spec.RefKind, Ref: spec.Ref, ObjectSHA: spec.SHA, CommitSHA: spec.SHA, Outcome: "queued"}
		events[id] = event
		body, err := json.Marshal(struct {
			Run                  model.RunSpec
			RequesterFingerprint string
			Supersede            bool
		}{Run: spec})
		if err != nil {
			t.Fatal(err)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = insertReceiveEvent(ctx, tx, event, "", unixNano(s.now())); err != nil {
			t.Fatal(err)
		}
		reason := "branch run deferred: queue at 1/1"
		if id == "deleted" {
			reason = refDeletedReason
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO deferred_branch_receives(event_id,run_spec,reason) VALUES(?,?,?)`, id, string(body), reason); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err = s.migrateWithMigrations(ctx, nil, migrations); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"admission_holds", "queue_priority_patterns", "run_priority_overrides", "run_priority_requests", "priority_grants", "queue_cap", "deferred_branch_receives", "size_grants", "resource_ceilings", "admission_capacity_override", "concurrency_group_declarations", "runs_repo_active_idx", "runs_repo_started_idx"} {
		var n int
		if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE name=?`, name).Scan(&n); err != nil || n != 0 {
			t.Fatal("retired schema", name, n, err)
		}
	}
	live, err := s.Run(ctx, running.ID)
	if err != nil || live.Status != model.RunRunning || live.Error != "Workflow create outcome uncertain; cleanup pending" {
		t.Fatal("running run changed", live, err)
	}
	pending, err := s.Run(ctx, queued.ID)
	if err != nil || pending.Status != model.RunQueued {
		t.Fatal("queued run changed", pending, err)
	}
	for _, id := range []string{"active", "deleted"} {
		replay, err := s.EnqueueReceiveEvent(ctx, events[id], saved[id])
		if err != nil || !replay.Duplicate || replay.ID == "" {
			t.Fatal("lost replay binding", id, replay, err)
		}
		want := model.RunQueued
		if id == "deleted" {
			want = model.RunInterrupted
		}
		if replay.Status != want {
			t.Fatal("wrong converted state", replay)
		}
	}
	// A deletion receipt never suppresses a new same-SHA push or cancels it on replay.
	event := events["deleted"]
	event.ID = "re-push"
	fresh, err := s.EnqueueReceiveEvent(ctx, event, saved["deleted"])
	if err != nil || fresh.Duplicate || fresh.Status != model.RunQueued {
		t.Fatal(fresh, err)
	}
	if _, err = s.EnqueueReceiveEvent(ctx, events["deleted"], saved["deleted"]); err != nil {
		t.Fatal(err)
	}
	latest, err := s.ResolveRun(ctx, repo.ID, saved["deleted"].SHA)
	if err != nil || latest.ID != fresh.ID {
		t.Fatal("cancelled receipt hid fresh run", latest, err)
	}
	for range 3 {
		if _, err = s.ClaimNextRun(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.FinishRun(ctx, fresh.ID, model.RunResult{Status: model.RunPassed, Phase: "complete"}); err != nil {
		t.Fatal(err)
	}
	latest, err = s.ResolveRun(ctx, repo.ID, saved["deleted"].SHA)
	if err != nil || latest.ID != fresh.ID || latest.Status != model.RunPassed {
		t.Fatal(latest, err)
	}
}
