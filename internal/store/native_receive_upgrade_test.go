package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func nativeReceiveUpgradeStore(t *testing.T) *Store {
	t.Helper()
	s := newMigrationIDTestStore(t)
	cut := -1
	for i, m := range migrations {
		if strings.Contains(m.id, "remove-run-fair-share") {
			cut = i
			break
		}
	}
	if cut < 0 {
		t.Fatal("missing native migration")
	}
	if err := s.migrateWithMigrations(t.Context(), nil, migrations[:cut]); err != nil {
		t.Fatal(err)
	}
	if err := s.configureLiveReaders(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	return s
}
func seedDeferredReceive(t *testing.T, s *Store, id string, spec model.RunSpec) model.ReceiveEventSpec {
	t.Helper()
	event := model.ReceiveEventSpec{ID: id, RepoID: spec.RepoID, RefKind: spec.RefKind, Ref: spec.Ref, Actor: spec.Actor, ObjectSHA: spec.SHA, CommitSHA: spec.SHA, Outcome: "queued"}
	raw, err := json.Marshal(struct {
		Run       model.RunSpec
		Supersede bool
	}{spec, true})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = insertReceiveEvent(t.Context(), tx, event, "", unixNano(s.now())); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(t.Context(), `INSERT INTO deferred_branch_receives(event_id,run_spec,reason) VALUES(?,?,'queue full')`, id, string(raw)); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return event
}
func applyNativeReceiveUpgrade(t *testing.T, s *Store) {
	t.Helper()
	for range 2 {
		if err := s.migrateWithMigrations(t.Context(), nil, migrations); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNativeReceiveUpgradeSupersedesOnlyMatchingScheduleLane(t *testing.T) {
	s := nativeReceiveUpgradeStore(t)
	repo := createRepo(t, s)
	ctx := t.Context()
	ordinary := testRunSpec(repo.ID, "feature/backlog", strings.Repeat("a", 40))
	previous, err := s.EnqueueRun(ctx, ordinary)
	if err != nil {
		t.Fatal(err)
	}
	nightly := ordinary
	nightly.Trigger = "schedule"
	nightly.ScheduleEntry = "nightly"
	scheduled, err := s.EnqueueRun(ctx, nightly)
	if err != nil {
		t.Fatal(err)
	}
	specs := map[string]model.RunSpec{}
	events := map[string]model.ReceiveEventSpec{}
	for i, id := range []string{"push-1", "push-2", "nightly-1", "nightly-2", "weekly-1"} {
		spec := ordinary
		spec.SHA = strings.Repeat(string(rune('b'+i)), 40)
		spec.TestedSHA = spec.SHA
		if strings.HasPrefix(id, "nightly") {
			spec.Trigger = "schedule"
			spec.ScheduleEntry = "nightly"
		}
		if strings.HasPrefix(id, "weekly") {
			spec.Trigger = "schedule"
			spec.ScheduleEntry = "weekly"
		}
		specs[id] = spec
		events[id] = seedDeferredReceive(t, s, id, spec)
	}
	applyNativeReceiveUpgrade(t, s)
	runs := map[string]model.Run{}
	for id, event := range events {
		result, err := s.EnqueueReceiveEvent(ctx, event, specs[id])
		if err != nil || !result.Duplicate {
			t.Fatal(id, result, err)
		}
		runs[id] = result.Run
	}
	check := func(id, successor string) {
		t.Helper()
		run, err := s.Run(ctx, id)
		if err != nil || run.Status != model.RunInterrupted || run.Phase != "interrupted" || run.Reason != "superseded by newer run" || run.SupersededBy != successor || run.FinishedAt == nil {
			t.Fatalf("incorrect supersession: %+v %v", run, err)
		}
	}
	check(previous.ID, runs["push-2"].ID)
	check(runs["push-1"].ID, runs["push-2"].ID)
	check(scheduled.ID, runs["nightly-2"].ID)
	check(runs["nightly-1"].ID, runs["nightly-2"].ID)
	for _, id := range []string{"push-2", "nightly-2", "weekly-1"} {
		if runs[id].Status != model.RunQueued || runs[id].ScheduleEntry != specs[id].ScheduleEntry {
			t.Fatal("lane lost", id, runs[id])
		}
	}
	for range 3 {
		if _, err = s.ClaimNextRun(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.ClaimNextRun(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatal("extra claimable superseded run", err)
	}
}

func TestNativeReceiveUpgradeArchivedReceiptsStayTerminalAfterUnarchive(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(map[bool]string{false: "archived", true: "unarchived-before-upgrade"}[active], func(t *testing.T) {
			s := nativeReceiveUpgradeStore(t)
			repo := createRepo(t, s)
			ctx := t.Context()
			if _, err := s.SetRepositoryArchived(ctx, "admin", repo.Name, true); err != nil {
				t.Fatal(err)
			}
			spec := testRunSpec(repo.ID, "feature/archived", strings.Repeat("a", 40))
			event := seedDeferredReceive(t, s, "archived-receive", spec)
			if active {
				if _, err := s.SetRepositoryArchived(ctx, "admin", repo.Name, false); err != nil {
					t.Fatal(err)
				}
			}
			applyNativeReceiveUpgrade(t, s)
			replay, err := s.EnqueueReceiveEvent(ctx, event, spec)
			if err != nil || !replay.Duplicate {
				t.Fatal(replay, err)
			}
			if active {
				if replay.Status != model.RunQueued {
					t.Fatal("latest unarchive ignored", replay)
				}
				return
			}
			if replay.Status != model.RunInterrupted || replay.Phase != "cancelled" || replay.Reason != "repository archived" {
				t.Fatal("archived receipt became active", replay)
			}
			if _, err = s.ClaimNextRun(ctx); !errors.Is(err, ErrNotFound) {
				t.Fatal("archived work claimable", err)
			}
			if _, err = s.SetRepositoryArchived(ctx, "admin", repo.Name, false); err != nil {
				t.Fatal(err)
			}
			again, err := s.EnqueueReceiveEvent(ctx, event, spec)
			if err != nil || again.ID != replay.ID || again.Status != model.RunInterrupted {
				t.Fatal("unarchive resurrected receipt", again, err)
			}
			event.ID = "new-push"
			fresh, err := s.EnqueueReceiveEvent(ctx, event, spec)
			if err != nil || fresh.Status != model.RunQueued || fresh.ID == replay.ID {
				t.Fatal("fresh push hidden", fresh, err)
			}
		})
	}
}
