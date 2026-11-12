package store

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestCanonicalPriorityMigrationPreservesRowsAndRejectsAliases(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	ctx := t.Context()
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "main", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Actor: "dev", Trigger: "push"})
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct the three pre-migration tables without changing the run or audit receipts.
	for _, table := range []string{"run_priority_overrides", "priority_grants", "run_priority_requests"} {
		if _, err := s.db.ExecContext(ctx, "DROP TABLE IF EXISTS "+table); err != nil {
			t.Fatal(err)
		}
	}
	_, err = s.db.ExecContext(ctx, `
 CREATE TABLE run_priority_overrides(run_id TEXT,priority TEXT,reason TEXT,actor TEXT,updated_at INTEGER);
 CREATE TABLE priority_grants(identity TEXT,max_class TEXT,expires INTEGER,granted_by TEXT);
 CREATE TABLE run_priority_requests(run_id TEXT,requested TEXT,accepted TEXT,reason TEXT,actor TEXT,refusal TEXT);
 INSERT INTO priority_grants VALUES('dev','highest',123,'admin');`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO run_priority_overrides VALUES(?,'normal','reason','admin',42)`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO run_priority_requests VALUES(?,'highest','highest','reason','dev','')`, run.ID); err != nil {
		t.Fatal(err)
	}
	body, err := migrationFiles.ReadFile("migrations/20261111200000-canonical-priorities.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, string(body)); err != nil {
		t.Fatal(err)
	}
	var priority, actor, requested, accepted, grant string
	var at int64
	if err := s.db.QueryRowContext(ctx, `SELECT priority,actor,updated_at FROM run_priority_overrides WHERE run_id=?`, run.ID).Scan(&priority, &actor, &at); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT requested,accepted FROM run_priority_requests WHERE run_id=?`, run.ID).Scan(&requested, &accepted); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT max_class FROM priority_grants WHERE identity='dev'`).Scan(&grant); err != nil {
		t.Fatal(err)
	}
	if priority != "standard" || requested != "high" || accepted != "high" || grant != "high" || actor != "admin" || at != 42 {
		t.Fatal("priority rows or metadata were not preserved")
	}
	for _, query := range []string{`UPDATE run_priority_overrides SET priority='normal'`, `UPDATE run_priority_requests SET accepted='highest'`, `UPDATE priority_grants SET max_class='highest'`} {
		if _, err := s.db.ExecContext(ctx, query); err == nil {
			t.Fatal("stored alias accepted")
		}
	}
}
