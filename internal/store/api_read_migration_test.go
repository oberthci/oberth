package store

import (
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestAPIReadIndexesUpgradePreservesHistory(t *testing.T) {
	const id = "20261112095139-api-read-indexes-5088d2686292709cb56a6d94bddb707b"
	s := newMigrationIDTestStore(t)
	ctx := t.Context()
	index := -1
	for i, migration := range migrations {
		if migration.id == id {
			index = i
		}
	}
	if index < 0 {
		t.Fatal("upgrade fixture must exclude the index migration")
	}
	before := migrations[:index]
	if err := s.migrateWithMigrations(ctx, nil, before); err != nil {
		t.Fatal(err)
	}
	if err := s.configureLiveReaders(ctx, 0); err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	enqueued, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/history", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	step := model.StepResult{RunID: enqueued.ID, Stage: "build", Step: "test", Status: model.StepPassed}
	if _, err := s.PutStepResult(ctx, step); err != nil {
		t.Fatal(err)
	}
	head, err := s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.Run(ctx, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := s.StepResults(ctx, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	existing := make(map[string]bool, len(before))
	for _, migration := range before {
		existing[migration.id] = true
	}
	ledger := func() string {
		rows, err := s.db.QueryContext(ctx, `SELECT id, version||':'||id||':'||applied_at FROM schema_migrations ORDER BY version`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var receipts []string
		for rows.Next() {
			var migrationID, receipt string
			if err := rows.Scan(&migrationID, &receipt); err != nil {
				t.Fatal(err)
			}
			if existing[migrationID] {
				receipts = append(receipts, receipt)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return strings.Join(receipts, "|")
	}
	oldLedger := ledger()
	for range 2 {
		if err := s.migrateWithMigrations(ctx, nil, migrations); err != nil {
			t.Fatal(err)
		}
	}
	after, err := s.VerifyAuditChain(ctx)
	if err != nil || !reflect.DeepEqual(head, after) {
		t.Fatal("audit changed", err)
	}
	got, err := s.Run(ctx, enqueued.ID)
	if err != nil || !reflect.DeepEqual(run, got) {
		t.Fatal("run changed", err)
	}
	gotSteps, err := s.StepResults(ctx, enqueued.ID)
	if err != nil || !reflect.DeepEqual(steps, gotSteps) {
		t.Fatal("steps changed", err)
	}
	if ledger() != oldLedger {
		t.Fatal("existing migration receipts changed")
	}
	var receipts, indexes int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE id=?`, id).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatal(receipts, err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type='index' AND name IN ('runs_counts_idx','runs_active_ref_idx','runs_terminal_end_idx','audit_actions_type_idx')`).Scan(&indexes); err != nil || indexes != 4 {
		t.Fatal(indexes, err)
	}
}
