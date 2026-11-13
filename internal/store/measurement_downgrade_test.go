package store

import (
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/testdb"
)

func TestMeasurementDowngradeMatchesHistoricalIndexInventoryAndReceipts(t *testing.T) {
	s := newMigrationIDTestStore(t)
	ctx := t.Context()
	prior := migrations[:len(migrations)-1]
	if err := s.migrateWithMigrations(ctx, nil, prior); err != nil {
		t.Fatal(err)
	}
	inventory := func() []string {
		rows, err := s.db.Query(`SELECT type||':'||name FROM sqlite_master
 WHERE tbl_name IN ('step_resource_usage','resource_attempts','resource_attempt_inventories','pod_resource_dispositions')
 AND type IN ('table','index') ORDER BY type,name`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var result []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			result = append(result, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return result
	}
	before := inventory()
	var receipt string
	if err := s.db.QueryRow(`SELECT id||':'||applied_at FROM schema_migrations WHERE version=?`, containerResourceUsageMigrationVersion).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	if err := s.migrateWithMigrations(ctx, nil, migrations); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(testdb.PostStepMeasurementDowngrade); err != nil {
		t.Fatal(err)
	}
	if after := inventory(); !reflect.DeepEqual(before, after) {
		t.Fatal("historical measurement table/index inventory differs", before, after)
	}
	var after string
	if err := s.db.QueryRow(`SELECT id||':'||applied_at FROM schema_migrations WHERE version=?`, containerResourceUsageMigrationVersion).Scan(&after); err != nil || after != receipt {
		t.Fatal("historical retention-index receipt changed", after, receipt, err)
	}
	var columns string
	if err := s.db.QueryRow(`SELECT group_concat(name,',') FROM pragma_index_info('step_resource_usage_retention_idx')`).Scan(&columns); err != nil || columns != "last_observed_at" {
		t.Fatal("wrong historical retention index", columns, err)
	}
	var removed int
	if err := s.db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE id=?`, migrations[len(migrations)-1].id).Scan(&removed); err != nil || removed != 0 {
		t.Fatal("measurement removal receipt not rewound", removed, err)
	}
	// Replay succeeds once and does not try to create the already-receipted
	// retention index a second time.
	for range 2 {
		if err := s.migrateWithMigrations(ctx, nil, migrations); err != nil {
			t.Fatal(err)
		}
	}
}
